package redirect_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"knative.dev/pkg/logging"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func upstreamResponse(status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func prometheusCounterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()

	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering prometheus metrics: %v", err)
	}
	for _, family := range metricFamilies {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if prometheusLabelsMatch(metric.GetLabel(), labels) {
				if metric.Counter == nil {
					t.Fatalf("%s is not a counter", name)
				}
				return metric.Counter.GetValue()
			}
		}
	}
	return 0
}

func prometheusLabelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	remaining := len(want)
	for _, label := range got {
		value, ok := want[label.GetName()]
		if ok && value == label.GetValue() {
			remaining--
		}
	}
	return remaining == 0
}

func newGuardedRedirect(transport http.RoundTripper, cacheBytes int64) http.Handler {
	return redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport: transport,
		RateLimit: redirect.RateLimitOptions{
			Disabled: true,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			MaxBytes: cacheBytes,
		},
		BlobCache: redirect.BlobCacheOptions{
			Disabled: true,
		},
	})
}

func manifestRequest(method, ref, accept string, withAuth bool) *http.Request {
	req := httptest.NewRequest(method, "/v2/engine/manifests/"+ref, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer test")
	}
	return req
}

func TestManifestCacheCachesGET(t *testing.T) {
	calls := 0
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if got, want := r.URL.String(), "https://ghcr.io/v2/dagger/engine/manifests/v1"; got != want {
			t.Fatalf("got upstream URL %q, want %q", got, want)
		}
		return upstreamResponse(http.StatusOK, http.Header{
			"Content-Type":          {"application/vnd.oci.image.index.v1+json"},
			"Docker-Content-Digest": {"sha256:abc"},
			"Etag":                  {`"sha256:abc"`},
		}, `{"schemaVersion":2}`), nil
	}), 1024)
	cacheHitsBefore := prometheusCounterValue(t, "registry_cache_hits_total", map[string]string{
		"registry":  "ghcr.io",
		"operation": "manifests",
		"method":    http.MethodGet,
	})

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", true))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", false))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached body = %q, want %q", second.Body.String(), first.Body.String())
	}
	if got := second.Header().Get("X-Redirected"); got != "https://ghcr.io/v2/dagger/engine/manifests/v1" {
		t.Fatalf("X-Redirected = %q", got)
	}
	if got := prometheusCounterValue(t, "registry_cache_hits_total", map[string]string{
		"registry":  "ghcr.io",
		"operation": "manifests",
		"method":    http.MethodGet,
	}) - cacheHitsBefore; got != 1 {
		t.Fatalf("manifest cache hit metric delta = %v, want 1", got)
	}
}

func TestManifestCacheLogsCacheHits(t *testing.T) {
	calls := 0
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return upstreamResponse(http.StatusOK, http.Header{
			"Content-Type":          {"application/vnd.oci.image.index.v1+json"},
			"Docker-Content-Digest": {"sha256:abc"},
			"Etag":                  {`"sha256:abc"`},
		}, `{"schemaVersion":2}`), nil
	}), 1024)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", true))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}

	core, observed := observer.New(zap.InfoLevel)
	req := manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", false)
	req = req.WithContext(logging.WithLogger(req.Context(), zap.New(core).Sugar()))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}

	entries := observed.FilterMessage("serving cached manifest").All()
	if len(entries) != 1 {
		t.Fatalf("cache hit log entries = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if got, want := fields["method"], http.MethodGet; got != want {
		t.Fatalf("logged method = %v, want %v", got, want)
	}
	if got, want := fields["url"], "/v2/engine/manifests/v1"; got != want {
		t.Fatalf("logged url = %v, want %v", got, want)
	}
	if got, want := fields["upstream_url"], "https://ghcr.io/v2/dagger/engine/manifests/v1"; got != want {
		t.Fatalf("logged upstream_url = %v, want %v", got, want)
	}
	if got, want := fields["status"], "200 OK"; got != want {
		t.Fatalf("logged status = %v, want %v", got, want)
	}
}

func TestManifestCacheKeysByAcceptHeader(t *testing.T) {
	calls := 0
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return upstreamResponse(http.StatusOK, http.Header{}, fmt.Sprintf(`{"call":%d}`, calls)), nil
	}), 1024)

	for _, accept := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, manifestRequest(http.MethodGet, "v1", accept, true))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestManifestCacheServesHEADAndConditionalRequests(t *testing.T) {
	calls := 0
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return upstreamResponse(http.StatusOK, http.Header{
			"Content-Length":        {"18"},
			"Content-Type":          {"application/vnd.oci.image.index.v1+json"},
			"Docker-Content-Digest": {"sha256:abc"},
			"Etag":                  {`"sha256:abc"`},
		}, `{"schemaVersion":2}`), nil
	}), 1024)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusOK)
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, manifestRequest(http.MethodHead, "v1", "application/vnd.oci.image.index.v1+json", false))
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", head.Code, http.StatusOK)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", head.Body.Len())
	}
	if got := head.Header().Get("Docker-Content-Digest"); got != "sha256:abc" {
		t.Fatalf("Docker-Content-Digest = %q", got)
	}

	conditional := manifestRequest(http.MethodGet, "v1", "application/vnd.oci.image.index.v1+json", false)
	conditional.Header.Set("If-None-Match", `"sha256:abc"`)
	notModified := httptest.NewRecorder()
	handler.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want %d", notModified.Code, http.StatusNotModified)
	}
	if notModified.Body.Len() != 0 {
		t.Fatalf("conditional body length = %d, want 0", notModified.Body.Len())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestManifestCacheDoesNotStoreNon200Responses(t *testing.T) {
	calls := 0
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return upstreamResponse(http.StatusNotFound, http.Header{
			"Content-Type": {"application/json"},
		}, `{"errors":[]}`), nil
	}), 1024)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, manifestRequest(http.MethodGet, "missing", "application/vnd.oci.image.index.v1+json", true))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestManifestCacheEvictsLRUWithinByteCap(t *testing.T) {
	calls := 0
	body := strings.Repeat("x", 100)
	handler := newGuardedRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return upstreamResponse(http.StatusOK, http.Header{}, body), nil
	}), 201)

	for _, ref := range []string{"v1", "v2", "v3", "v1"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, manifestRequest(http.MethodGet, ref, "", true))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", ref, rec.Code, http.StatusOK)
		}
	}
	if calls != 4 {
		t.Fatalf("upstream calls = %d, want 4 because v1 was evicted", calls)
	}
}

func TestIPRateLimiterRejectsNoisyClients(t *testing.T) {
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		RateLimit: redirect.RateLimitOptions{
			RequestsPerMinute: 1,
			Burst:             2,
			IdleTTL:           time.Minute,
			MaxIPs:            10,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limited status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if !strings.Contains(rec.Body.String(), "TOOMANYREQUESTS") {
		t.Fatalf("rate-limit body = %q", rec.Body.String())
	}
}

func TestIPRateLimiterRecordsRateLimitedClients(t *testing.T) {
	tracker := redirect.NewRateLimitedIPTracker(50)
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		RateLimit: redirect.RateLimitOptions{
			RequestsPerMinute: 1,
			Burst:             1,
			IdleTTL:           time.Minute,
			MaxIPs:            10,
			LimitedIPs:        tracker,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("first status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	if got := tracker.TopMap(); len(got) != 0 {
		t.Fatalf("tracker after allowed request = %#v, want empty", got)
	}

	for i := 0; i < 2; i++ {
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("limited request %d status = %d, want %d", i, rec.Code, http.StatusTooManyRequests)
		}
	}

	got := tracker.TopMap()
	if got["203.0.113.10"] != 2 {
		t.Fatalf("tracked count = %#v, want 203.0.113.10 counted twice", got)
	}
}

func TestIPRateLimiterUsesOverrideForConfiguredIPRanges(t *testing.T) {
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		RateLimit: redirect.RateLimitOptions{
			RequestsPerMinute: 1,
			Burst:             1,
			IdleTTL:           time.Minute,
			MaxIPs:            10,
			IPOverrides: []redirect.IPRateLimitOverride{{
				RequestsPerMinute: 2,
				Burst:             2,
				IPPrefixes: []netip.Prefix{
					netip.MustParsePrefix("203.0.113.0/24"),
				},
			}},
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.10:1234"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("configured request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("configured limited status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("default first status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("default limited status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
}

func TestIPRateLimiterSkipsBlobRequests(t *testing.T) {
	calls := 0
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if !strings.Contains(r.URL.Path, "/blobs/") {
				t.Fatalf("upstream path = %q, want blob request", r.URL.Path)
			}
			return upstreamResponse(http.StatusTemporaryRedirect, http.Header{
				"Location": {"https://example.test/blob"},
			}, ""), nil
		}),
		RateLimit: redirect.RateLimitOptions{
			RequestsPerMinute: 1,
			Burst:             1,
			IdleTTL:           time.Minute,
			MaxIPs:            10,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
	})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/engine/blobs/sha256:abc", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		req.Header.Set("Authorization", "Bearer test")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("blob request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
	}
	if calls != 3 {
		t.Fatalf("upstream blob calls = %d, want 3", calls)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("first non-blob status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second non-blob status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
}

func TestBlobRedirectCacheCachesSignedGETRedirects(t *testing.T) {
	expiresAt := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	location := "https://pkg-containers.githubusercontent.com/ghcrblobs16/blobs/sha256:abc?se=" +
		url.QueryEscape(expiresAt) + "&sig=one"
	calls := 0
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return upstreamResponse(http.StatusTemporaryRedirect, http.Header{
				"Content-Length": {"0"},
				"Location":       {location},
			}, ""), nil
		}),
		RateLimit: redirect.RateLimitOptions{
			Disabled: true,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
		BlobCache: redirect.BlobCacheOptions{
			MaxBytes: 1024 * 1024,
		},
	})
	cacheHitsBefore := prometheusCounterValue(t, "registry_cache_hits_total", map[string]string{
		"registry":  "ghcr.io",
		"operation": "blobs",
		"method":    http.MethodGet,
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/engine/blobs/sha256:abc", nil)
		req.Header.Set("Authorization", "Bearer test")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("blob request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
		if got := rec.Header().Get("Location"); got != location {
			t.Fatalf("blob request %d Location = %q, want %q", i, got, location)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream blob calls = %d, want 1", calls)
	}
	if got := prometheusCounterValue(t, "registry_cache_hits_total", map[string]string{
		"registry":  "ghcr.io",
		"operation": "blobs",
		"method":    http.MethodGet,
	}) - cacheHitsBefore; got != 1 {
		t.Fatalf("blob cache hit metric delta = %v, want 1", got)
	}
}

func TestBlobRedirectCacheSkipsRedirectsWithoutSignedExpiry(t *testing.T) {
	calls := 0
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return upstreamResponse(http.StatusTemporaryRedirect, http.Header{
				"Location": {"https://pkg-containers.githubusercontent.com/ghcrblobs16/blobs/sha256:abc?sig=one"},
			}, ""), nil
		}),
		RateLimit: redirect.RateLimitOptions{
			Disabled: true,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
		BlobCache: redirect.BlobCacheOptions{
			MaxBytes: 1024 * 1024,
		},
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/engine/blobs/sha256:abc", nil)
		req.Header.Set("Authorization", "Bearer test")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("blob request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream blob calls = %d, want 2", calls)
	}
}

func TestBlobRedirectCacheSkipsRedirectsTooCloseToExpiry(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)
	location := "https://pkg-containers.githubusercontent.com/ghcrblobs16/blobs/sha256:abc?se=" +
		url.QueryEscape(expiresAt) + "&sig=one"
	calls := 0
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return upstreamResponse(http.StatusTemporaryRedirect, http.Header{
				"Location": {location},
			}, ""), nil
		}),
		RateLimit: redirect.RateLimitOptions{
			Disabled: true,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
		BlobCache: redirect.BlobCacheOptions{
			MaxBytes: 1024 * 1024,
		},
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/engine/blobs/sha256:abc", nil)
		req.Header.Set("Authorization", "Bearer test")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("blob request %d status = %d, want %d", i, rec.Code, http.StatusTemporaryRedirect)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream blob calls = %d, want 2", calls)
	}
}

func TestIPRateLimiterUsesFlyClientIPBeforeForwardedHeaders(t *testing.T) {
	handler := redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		RateLimit: redirect.RateLimitOptions{
			RequestsPerMinute: 1,
			Burst:             1,
			IdleTTL:           time.Minute,
			MaxIPs:            10,
		},
		ManifestCache: redirect.ManifestCacheOptions{
			Disabled: true,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Fly-Client-IP", "203.0.113.10")
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("first status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.2:1234"
	req.Header.Set("Fly-Client-IP", "203.0.113.10")
	req.Header.Set("X-Forwarded-For", "198.51.100.11")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("same Fly-Client-IP status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Fly-Client-IP", "203.0.113.11")
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("different Fly-Client-IP status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
}
