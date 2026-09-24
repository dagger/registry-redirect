package redirect_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
)

func TestAnonymousTokenCacheAdmission(t *testing.T) {
	for _, path := range []string{clientTokenPath, "/v2/engine/manifests/v1"} {
		for _, tc := range []struct {
			name, body          string
			status, proxyStatus int
			calls               int32
		}{
			{"valid", `{"token":"test"}`, http.StatusOK, http.StatusOK, 1},
			{"forbidden", `{}`, http.StatusForbidden, http.StatusForbidden, 2},
			{"malformed", `{"token":`, http.StatusOK, http.StatusInternalServerError, 2},
			{"oversized", `{"token":"` + strings.Repeat("x", 64*1024) + `"}`, http.StatusOK, http.StatusOK, 2},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var tokens, manifests atomic.Int32
				handler := anonymousRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/token" {
						tokens.Add(1)
						return upstreamResponse(tc.status, nil, tc.body), nil
					}
					manifests.Add(1)
					return upstreamResponse(http.StatusOK, nil, `{}`), nil
				}), noRateLimit())
				want := tc.status
				if path != clientTokenPath {
					want = tc.proxyStatus
				}
				for i := 0; i < 2; i++ {
					rec := serveRequest(t, handler, httptest.NewRequest(http.MethodGet, path, nil), want)
					if path == clientTokenPath && rec.Body.String() != tc.body {
						t.Fatal("token response was not relayed intact")
					}
				}
				if tokens.Load() != tc.calls {
					t.Fatalf("token fetches = %d, want %d", tokens.Load(), tc.calls)
				}
				if want != http.StatusOK && manifests.Load() != 0 {
					t.Fatal("failed token fetch reached the manifest endpoint")
				}
			})
		}
	}
}

func TestAnonymousTokenCacheKey(t *testing.T) {
	var up tokenUpstream
	handler := anonymousRedirect(up.transport(), noRateLimit())
	for _, query := range []string{
		"scope=repository:engine:pull&service=ghcr.io",
		"service=ghcr.io&scope=repository%3Aengine%3Apull",
		"scope=repository:other:pull&service=ghcr.io",
		"scope=repository:engine:pull&service=other",
		"scope=repository:engine:pull&service=ghcr.io&account=someone",
	} {
		serveRequest(t, handler, httptest.NewRequest(http.MethodGet, "/token?"+query, nil), http.StatusOK)
	}
	if up.tokens.Load() != 4 {
		t.Fatalf("token fetches = %d, want 4 distinct queries", up.tokens.Load())
	}
	// Proxy-generated scopes must agree with client-generated scopes.
	for _, repo := range []string{"engine", "other"} {
		serveRequest(t, handler, httptest.NewRequest(http.MethodGet, "/v2/"+repo+"/manifests/v1", nil), http.StatusOK)
	}
	if up.tokens.Load() != 4 {
		t.Fatal("proxy did not reuse the scoped client tokens")
	}
}

func TestTokenCacheBypass(t *testing.T) {
	for _, tc := range []struct{ name, method, auth string }{
		{"authenticated", http.MethodGet, "Basic dXNlcjpwYXNz"},
		{"POST", http.MethodPost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			handler := anonymousRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				token := "anonymous"
				if r.Method == tc.method && r.Header.Get("Authorization") == tc.auth {
					token = "private"
				}
				return upstreamResponse(http.StatusOK, nil, fmt.Sprintf(`{"token":%q}`, token)), nil
			}), noRateLimit())
			send := func(method, auth, want string) {
				rec := serveRequest(t, handler, clientTokenRequest(method, auth, "203.0.113.1"), http.StatusOK)
				if rec.Body.String() != fmt.Sprintf(`{"token":%q}`, want) {
					t.Fatalf("token response = %s, want %s", rec.Body.String(), want)
				}
			}
			// Bypasses neither seed nor read the anonymous cache.
			send(tc.method, tc.auth, "private")
			send(http.MethodGet, "", "anonymous")
			send(tc.method, tc.auth, "private")
			send(http.MethodGet, "", "anonymous")
			if calls.Load() != 3 {
				t.Fatalf("upstream calls = %d, want 3", calls.Load())
			}
		})
	}
	var up tokenUpstream
	handler := anonymousRedirect(up.transport(), noRateLimit())
	serveRequest(t, handler, manifestFrom("203.0.113.1", "v1", true), http.StatusOK)
	if up.tokens.Load() != 0 {
		t.Fatal("authenticated manifest fetched an anonymous token")
	}
}

func TestTokenCacheCharging(t *testing.T) {
	for _, tc := range []struct {
		name, path, operation string
		burst                 int
		statuses              []int
		manifests             int32
		cacheHits             float64
	}{
		{"client cache hits are free", clientTokenPath, "token", 1, []int{200, 200, 200}, 0, 2},
		{"proxy still charges manifest", "/v2/engine/manifests/v1", "auth_token", 2, []int{200, 200, 429}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var up tokenUpstream
			handler := anonymousRedirect(up.transport(), rateLimit(tc.burst))
			labels := map[string]string{"registry": "ghcr.io", "operation": tc.operation, "method": http.MethodGet}
			before := prometheusCounterValue(t, "registry_cache_hits_total", labels)
			for _, want := range tc.statuses {
				serveRequest(t, handler, httptest.NewRequest(http.MethodGet, tc.path, nil), want)
			}
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer private")
			assertRateLimited(t, serveRequest(t, handler, req, http.StatusTooManyRequests))
			if up.tokens.Load() != 1 || up.manifests.Load() != tc.manifests {
				t.Fatalf("upstream: tokens=%d manifests=%d, want 1 and %d", up.tokens.Load(), up.manifests.Load(), tc.manifests)
			}
			if hits := prometheusCounterValue(t, "registry_cache_hits_total", labels) - before; hits != tc.cacheHits {
				t.Fatalf("cache hits = %v, want %v", hits, tc.cacheHits)
			}
		})
	}
}

type tokenUpstream struct {
	tokens    atomic.Int32
	manifests atomic.Int32
}

func (u *tokenUpstream) transport() roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			u.tokens.Add(1)
			return upstreamResponse(http.StatusOK, http.Header{
				"Content-Type": {"application/json"},
			}, `{"token":"test"}`), nil // no expires_in, exactly like GHCR
		}
		u.manifests.Add(1)
		return upstreamResponse(http.StatusOK, http.Header{
			"Content-Type": {"application/vnd.oci.image.index.v1+json"},
		}, `{"schemaVersion":2}`), nil
	}
}

// anonymousRedirect keeps the token cache on (the production default) and
// the manifest cache off, so every pull's manifest goes upstream and only
// token behaviour varies between requests.
func anonymousRedirect(transport http.RoundTripper, rateLimit redirect.RateLimitOptions) http.Handler {
	return redirect.NewWithOptions("ghcr.io", "dagger", "", redirect.Options{
		Transport:     transport,
		RateLimit:     rateLimit,
		ManifestCache: redirect.ManifestCacheOptions{Disabled: true},
		BlobCache:     redirect.BlobCacheOptions{Disabled: true},
	})
}

func noRateLimit() redirect.RateLimitOptions {
	return redirect.RateLimitOptions{Disabled: true}
}

func rateLimit(burst int) redirect.RateLimitOptions {
	return redirect.RateLimitOptions{RequestsPerMinute: 1, Burst: burst, IdleTTL: time.Minute, MaxIPs: 10}
}

const clientTokenPath = "/token?scope=repository:engine:pull&service=ghcr.io"

func clientTokenRequest(method, auth, ip string) *http.Request {
	req := httptest.NewRequest(method, clientTokenPath, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.RemoteAddr = ip + ":1234"
	return req
}

func TestAnonymousTokenUsesCanonicalHeaders(t *testing.T) {
	for _, firstPath := range []string{clientTokenPath, "/v2/engine/manifests/v1"} {
		t.Run(firstPath, func(t *testing.T) {
			var calls atomic.Int32
			handler := anonymousRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/token" {
					if got := r.Header.Get("Authorization"); got != "Bearer test" {
						t.Errorf("manifest Authorization = %q, want Bearer test", got)
					}
					return upstreamResponse(http.StatusOK, nil, `{}`), nil
				}
				calls.Add(1)
				if r.Header.Get("Accept") != "application/json" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("token representation headers = %v, want JSON and identity encoding", r.Header)
				}
				for _, key := range []string{"Cookie", "Range", "If-None-Match"} {
					if r.Header.Get(key) != "" {
						t.Errorf("anonymous token fetch forwarded %s", key)
					}
				}
				resp := upstreamResponse(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, `{"token":"test"}`)
				if r.Header.Get("Accept-Encoding") == "gzip" {
					var buf bytes.Buffer
					z := gzip.NewWriter(&buf)
					_, _ = z.Write([]byte(`{"token":"test"}`))
					_ = z.Close()
					resp.Body = io.NopCloser(&buf)
					resp.Header.Set("Content-Encoding", "gzip")
				}
				return resp, nil
			}), noRateLimit())

			first := httptest.NewRequest(http.MethodGet, firstPath, nil)
			first.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
			first.Header.Set("Accept-Encoding", "gzip")
			first.Header.Set("Cookie", "session=client-specific")
			first.Header.Set("Range", "bytes=0-1")
			first.Header.Set("If-None-Match", `"client-specific"`)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, first)
			if rec.Code != http.StatusOK {
				t.Fatalf("first response = %d: %s", rec.Code, rec.Body.String())
			}

			second := httptest.NewRequest(http.MethodGet, clientTokenPath, nil)
			second.Header.Set("Accept-Encoding", "identity")
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, second)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != `{"token":"test"}` {
				t.Fatalf("identity client received status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("token fetches = %d, want 1 across both paths", got)
			}
		})
	}
}

// Both paths share a canonical upstream URL, whether the client or proxy
// populates the cache first. Repository and prefix rewriting must agree.
func TestClientAndProxyShareTokenCache(t *testing.T) {
	for _, host := range []string{"ghcr.io", "gcr.io"} {
		for _, prefix := range []string{"", "images/"} {
			for _, clientFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/client-first=%v", host, prefix, clientFirst), func(t *testing.T) {
					var calls atomic.Int32
					handler := redirect.NewWithOptions(host, "dagger", strings.TrimSuffix(prefix, "/"), redirect.Options{
						RateLimit: noRateLimit(),
						Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
							if strings.HasSuffix(r.URL.Path, "/token") {
								calls.Add(1)
								if got := r.URL.Query().Get("scope"); got != "repository:dagger/engine:pull" {
									t.Errorf("scope = %q", got)
								}
								return upstreamResponse(http.StatusOK, nil, `{"token":"test"}`), nil
							}
							if r.Header.Get("Authorization") != "Bearer test" {
								t.Errorf("manifest did not receive the cached token")
							}
							return upstreamResponse(http.StatusOK, nil, `{}`), nil
						}),
					})
					paths := []string{
						"/token?service=" + host + "&scope=repository:" + prefix + "engine:pull",
						"/v2/" + prefix + "engine/manifests/v1",
					}
					if !clientFirst {
						paths[0], paths[1] = paths[1], paths[0]
					}
					for _, path := range paths {
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
						if rec.Code != http.StatusOK {
							t.Fatalf("%s: status = %d, body = %s", path, rec.Code, rec.Body.String())
						}
					}
					if got := calls.Load(); got != 1 {
						t.Fatalf("token fetches across both paths = %d, want 1", got)
					}
				})
			}
		}
	}
}
