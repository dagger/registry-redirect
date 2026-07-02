package redirect

import (
	"context"
	"net/http"
	"net/url"
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

func TestBackendOperation(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"v2", "/v2/", "v2"},
		{"ghcr token", "/token", "token"},
		{"gcr token", "/v2/token", "token"},
		{"manifest", "/v2/dagger/engine/manifests/latest", "manifests"},
		{"blob", "/v2/dagger/engine/blobs/sha256:abc", "blobs"},
		{"tags", "/v2/dagger/engine/tags/list", "tags"},
		{"unknown", "/v2/dagger/engine/referrers/sha256:abc", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backendOperation(tc.path); got != tc.want {
				t.Fatalf("backendOperation(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestDefaultOptionsSetsClientTimeout(t *testing.T) {
	opts := DefaultOptions()
	if opts.Client.Timeout != defaultBackendRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", opts.Client.Timeout, defaultBackendRequestTimeout)
	}
	if opts.BlobCache.MaxBytes != defaultBlobCacheMaxBytes {
		t.Fatalf("blob cache max bytes = %d, want %d", opts.BlobCache.MaxBytes, defaultBlobCacheMaxBytes)
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

func TestBlobRedirectCacheTTL(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 18, 0, 0, time.UTC)
	location := func(expiry time.Time) string {
		return "https://pkg-containers.githubusercontent.com/ghcrblobs16/blobs/sha256:abc?se=" +
			url.QueryEscape(expiry.Format(time.RFC3339)) + "&sig=test"
	}

	for _, tc := range []struct {
		name     string
		location string
		want     time.Duration
		wantOK   bool
	}{
		{
			name:     "caps long signed URL lifetime",
			location: location(now.Add(10 * time.Minute)),
			want:     blobRedirectCacheMaxTTL,
			wantOK:   true,
		},
		{
			name:     "subtracts expiry margin",
			location: location(now.Add(3 * time.Minute)),
			want:     2 * time.Minute,
			wantOK:   true,
		},
		{
			name:     "rejects expiry inside margin",
			location: location(now.Add(30 * time.Second)),
			wantOK:   false,
		},
		{
			name:     "rejects missing expiry",
			location: "https://pkg-containers.githubusercontent.com/ghcrblobs16/blobs/sha256:abc?sig=test",
			wantOK:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := blobRedirectCacheTTL(tc.location, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("ttl = %s, want %s", got, tc.want)
			}
		})
	}
}
