package redirect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func asyncRequest(handler http.Handler, req *http.Request) <-chan int {
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	return done
}

func TestAnonymousTokenWaitCanBeCanceled(t *testing.T) {
	for _, path := range []string{clientTokenPath, "/v2/engine/manifests/v1"} {
		for cancelIndex, role := range []string{"leader", "follower"} {
			t.Run(path+"/"+role, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					release := make(chan struct{})
					defer close(release)
					var tokens atomic.Int32
					handler := anonymousRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path != "/token" {
							return upstreamResponse(http.StatusOK, nil, `{}`), nil
						}
						tokens.Add(1)
						select {
						case <-release:
							return upstreamResponse(http.StatusOK, nil, `{"token":"test"}`), nil
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}), noRateLimit())
					canceledCtx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var requests [2]<-chan int
					for i := range requests {
						ctx := t.Context()
						if i == cancelIndex {
							ctx = canceledCtx
						}
						requests[i] = asyncRequest(handler, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
						synctest.Wait() // Both callers must join before either is canceled.
					}
					cancel()
					synctest.Wait()
					select {
					case status := <-requests[cancelIndex]:
						if status == http.StatusOK {
							t.Fatal("canceled request succeeded before upstream was released")
						}
					default:
						t.Fatal("canceled request is still waiting for the shared fetch")
					}
					release <- struct{}{}
					if status := <-requests[1-cancelIndex]; status != http.StatusOK {
						t.Fatalf("surviving request = %d, want 200", status)
					}
					if got := tokens.Load(); got != 1 {
						t.Fatalf("token fetches = %d, want 1", got)
					}
				})
			})
		}
	}
}

func TestAnonymousTokenSharedFlightChargesEachCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		var tokens, manifests atomic.Int32
		handler := anonymousRedirect(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/token" {
				tokens.Add(1)
				<-release
				return upstreamResponse(http.StatusOK, nil, `{"token":"test"}`), nil
			}
			manifests.Add(1)
			return upstreamResponse(http.StatusOK, nil, `{}`), nil
		}), rateLimit(1))

		leader := asyncRequest(handler, clientTokenRequest(http.MethodGet, "", "203.0.113.1"))
		synctest.Wait()
		follower := asyncRequest(handler, manifestFrom("203.0.113.2", "v1", false))
		synctest.Wait()
		// The proxy caller must spend its own IP's allowance before joining
		// the client's fetch, so a third request from that IP is refused.
		assertRateLimited(t, serveRequest(t, handler, clientTokenRequest(http.MethodGet, "", "203.0.113.2"), http.StatusTooManyRequests))
		release <- struct{}{}
		for _, done := range []<-chan int{leader, follower} {
			if status := <-done; status != http.StatusOK {
				t.Fatalf("permitted caller returned %d, want 200", status)
			}
		}
		if tokens.Load() != 1 || manifests.Load() != 1 {
			t.Fatalf("upstream calls: tokens=%d manifests=%d, want 1 each", tokens.Load(), manifests.Load())
		}
	})
}
