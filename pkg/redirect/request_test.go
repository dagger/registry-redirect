package redirect

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestNewBackendRequestUsesParentContext(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	req, err := newBackendRequest(parent, http.MethodGet, "https://ghcr.io/v2/", nil)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	cancelParent()

	select {
	case <-req.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled after parent context cancellation")
	}
}

func TestNewBackendRequestClonesHeaders(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer original"}}

	req, err := newBackendRequest(context.Background(), http.MethodGet, "https://ghcr.io/v2/", headers)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	headers.Set("Authorization", "Bearer changed")

	if got := req.Header.Get("Authorization"); got != "Bearer original" {
		t.Fatalf("Authorization = %q, want original cloned value", got)
	}
}

func TestDefaultOptionsSetsClientTimeout(t *testing.T) {
	opts := DefaultOptions()
	if opts.Client.Timeout != defaultBackendRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", opts.Client.Timeout, defaultBackendRequestTimeout)
	}
}

func TestOptionsWithDefaultsCopiesCustomClientBeforeSettingTimeout(t *testing.T) {
	custom := &http.Client{}

	opts := (Options{Client: custom}).withDefaults()

	if opts.Client == custom {
		t.Fatal("withDefaults reused custom client while setting timeout")
	}
	if opts.Client.Timeout != defaultBackendRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", opts.Client.Timeout, defaultBackendRequestTimeout)
	}
	if custom.Timeout != 0 {
		t.Fatalf("custom client timeout was mutated to %s", custom.Timeout)
	}
}
