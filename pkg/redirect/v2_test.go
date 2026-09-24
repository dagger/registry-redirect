package redirect_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func v2Upstream(calls *atomic.Int32, status int, body string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return upstreamResponse(status, http.Header{
			"Www-Authenticate": {`Bearer realm="https://ghcr.io/token",service="ghcr.io"`},
		}, body), nil
	}
}

func TestV2CacheRewritesRealmWithoutChargingHits(t *testing.T) {
	var calls atomic.Int32
	const body = `{"errors":[{"code":"UNAUTHORIZED"}]}`
	handler := anonymousRedirect(v2Upstream(&calls, http.StatusUnauthorized, body), rateLimit(1))
	labels := map[string]string{"registry": "ghcr.io", "operation": "v2", "method": http.MethodGet}
	before := prometheusCounterValue(t, "registry_cache_hits_total", labels)
	for _, host := range []string{"registry.dagger.io", "canary.dagger.io", "registry.dagger.io"} {
		req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		req.Host = host
		rec := serveRequest(t, handler, req, http.StatusUnauthorized)
		want := `Bearer realm="https://` + host + `/token",service="ghcr.io"`
		if rec.Header().Get("Www-Authenticate") != want || rec.Body.String() != body {
			t.Fatalf("%s: headers=%v body=%q", host, rec.Header(), rec.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if hits := prometheusCounterValue(t, "registry_cache_hits_total", labels) - before; hits != 2 {
		t.Fatalf("cache hits = %v, want 2", hits)
	}
}

func TestV2CacheAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
		calls              int32
	}{
		{"success", http.MethodGet, `{}`, http.StatusOK, 1},
		{"challenge", http.MethodGet, `{}`, http.StatusUnauthorized, 1},
		{"failure", http.MethodGet, `{}`, http.StatusServiceUnavailable, 2},
		{"forbidden", http.MethodGet, `{}`, http.StatusForbidden, 2},
		{"HEAD", http.MethodHead, "", http.StatusUnauthorized, 2},
		{"oversized", http.MethodGet, strings.Repeat("x", 64*1024+1), http.StatusOK, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			handler := anonymousRedirect(v2Upstream(&calls, tc.status, tc.body), noRateLimit())
			// A GET primes eligible responses; HEAD must still go live afterwards.
			serveRequest(t, handler, httptest.NewRequest(http.MethodGet, "/v2/", nil), tc.status)
			rec := serveRequest(t, handler, httptest.NewRequest(tc.method, "/v2/", nil), tc.status)
			if calls.Load() != tc.calls || rec.Body.String() != tc.body {
				t.Fatalf("calls=%d body length=%d, want %d and %d", calls.Load(), rec.Body.Len(), tc.calls, len(tc.body))
			}
		})
	}
}

func TestV2CacheConcurrentHosts(t *testing.T) {
	var calls atomic.Int32
	handler := anonymousRedirect(v2Upstream(&calls, http.StatusUnauthorized, `{}`), noRateLimit())
	counts := burstRequests(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := `Bearer realm="https://` + req.Host + `/token",service="ghcr.io"`
		if rec.Header().Get("Www-Authenticate") != want {
			t.Errorf("%s: realm = %q", req.Host, rec.Header().Get("Www-Authenticate"))
		}
		w.WriteHeader(rec.Code)
	}), 20, func(i int) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		req.Host = fmt.Sprintf("host-%d.example", i)
		return req
	})
	if counts[http.StatusUnauthorized] != 20 {
		t.Fatalf("statuses = %v", counts)
	}
	before := calls.Load()
	serveRequest(t, handler, httptest.NewRequest(http.MethodGet, "/v2/", nil), http.StatusUnauthorized)
	if calls.Load() != before {
		t.Fatal("post-burst GET missed the cache")
	}
}
