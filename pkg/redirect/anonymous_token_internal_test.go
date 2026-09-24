package redirect

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type tokenTransport func(*http.Request) (*http.Response, error)

func (f tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnonymousTokenRechecksCacheAfterMiss(t *testing.T) {
	var calls int
	rdr := &redirect{
		host:       "ghcr.io",
		tokenCache: newResponseCache(time.Minute, 10),
		client: &http.Client{Timeout: time.Second, Transport: tokenTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"token":"test"}`))}, nil
		})},
	}
	const url = "https://ghcr.io/token?scope=repository%3Adagger%2Fengine%3Apull&service=ghcr.io"
	if _, ok := rdr.cachedToken(url, "token"); ok {
		t.Fatal("expected a cache miss")
	}
	// One caller fills the cache before the other starts its fetch. The late
	// caller must recheck inside singleflight, even though its lookup missed.
	for _, operation := range []string{"auth_token", "token"} {
		if _, err := rdr.fetchAnonymousToken(t.Context(), url, operation); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}
