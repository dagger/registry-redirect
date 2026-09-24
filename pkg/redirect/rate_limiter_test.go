package redirect_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestIPRateLimiterCacheHitsAreFree(t *testing.T) {
	var calls atomic.Int32
	handler := limitedRedirect(okUpstream(&calls), 1, 1024, nil)
	serveRequest(t, handler, manifestFrom("203.0.113.1", "v1", true), http.StatusOK)

	// The warm-up exhausted the bucket. All concurrent cache hits must succeed.
	counts := burstRequests(t, handler, 20, func(int) *http.Request {
		return manifestFrom("203.0.113.1", "v1", false)
	})
	if counts[http.StatusOK] != 20 {
		t.Fatalf("warm burst statuses = %v, want 20 successes", counts)
	}
	conditional := manifestFrom("203.0.113.1", "v1", false)
	conditional.Header.Set("If-None-Match", `"sha256:abc"`)
	serveRequest(t, handler, conditional, http.StatusNotModified)
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want only the warm-up", calls.Load())
	}
}

func TestIPRateLimiterConcurrentColdPulls(t *testing.T) {
	var calls atomic.Int32
	handler := limitedRedirect(okUpstream(&calls), 3, 0, nil)
	counts := burstRequests(t, handler, 20, func(i int) *http.Request {
		return manifestFrom("203.0.113.1", fmt.Sprintf("v%d", i), true)
	})
	if counts[http.StatusOK] != 3 || counts[http.StatusTooManyRequests] != 17 || calls.Load() != 3 {
		t.Fatalf("cold burst: statuses=%v upstream=%d, want 3 successes, 17 refusals and 3 upstream calls", counts, calls.Load())
	}
}

func TestIPRateLimiterChargesUpstreamRequestsOnce(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		upstreamCalls      int32
	}{
		{"anonymous pull", http.MethodGet, "/v2/engine/manifests/v1", 2},
		{"HEAD", http.MethodHead, "/v2/engine/manifests/v1", 2},
		{"challenge", http.MethodGet, "/v2/", 1},
		{"client token", http.MethodGet, "/token?scope=repository:engine:pull", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			handler := limitedRedirect(okUpstream(&calls), 1, 1024, nil)
			request := func() *http.Request { return httptest.NewRequest(tc.method, tc.path, nil) }
			serveRequest(t, handler, request(), http.StatusOK)
			// Use a new ref so the GET is cold even with manifest caching enabled.
			req := request()
			if tc.name == "anonymous pull" {
				req.URL.Path = "/v2/engine/manifests/v2"
			}
			assertRateLimited(t, serveRequest(t, handler, req, http.StatusTooManyRequests))
			if calls.Load() != tc.upstreamCalls {
				t.Fatalf("upstream calls = %d, want %d; refused requests must not reach upstream", calls.Load(), tc.upstreamCalls)
			}
		})
	}
}

func TestIPRateLimiterLocalRoutesAreFree(t *testing.T) {
	var calls atomic.Int32
	handler := limitedRedirect(okUpstream(&calls), 1, 0, nil)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/", http.StatusTemporaryRedirect}, {"/missing", http.StatusNotFound},
	} {
		for i := 0; i < 3; i++ {
			serveRequest(t, handler, httptest.NewRequest(http.MethodGet, tc.path, nil), tc.status)
		}
	}
	// Local responses must neither contact upstream nor consume its allowance.
	if calls.Load() != 0 {
		t.Fatalf("local routes made %d upstream calls", calls.Load())
	}
	serveRequest(t, handler, httptest.NewRequest(http.MethodGet, "/v2/engine/manifests/v1", nil), http.StatusOK)
}

func TestUpstreamTimeoutsReturnGatewayTimeout(t *testing.T) {
	for _, path := range []string{"/v2/", "/token", "/v2/engine/manifests/v1"} {
		for _, authenticated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/auth=%v", path, authenticated), func(t *testing.T) {
				var calls atomic.Int32
				handler := newGuardedRedirect(roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, timeoutError{}
				}), 1024)
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if authenticated {
					req.Header.Set("Authorization", "Bearer test")
				}
				serveRequest(t, handler, req, http.StatusGatewayTimeout)
				if calls.Load() != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls.Load())
				}
			})
		}
	}
}
