package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
	dto "github.com/prometheus/client_model/go"
)

func TestMetricPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"root", "/", "/"},
		{"v2", "/v2", "/v2"},
		{"v2 slash", "/v2/", "/v2"},
		{"token", "/token", "/token"},
		{"manifest", "/v2/dagger/engine/manifests/main", "/v2/{repo}/manifests/{tagOrDigest}"},
		{"manifest with prefix", "/v2/prefix/engine/manifests/sha256:abc", "/v2/{repo}/manifests/{tagOrDigest}"},
		{"blob", "/v2/dagger/engine/blobs/sha256:abc", "/v2/{repo}/blobs/{digest}"},
		{"tags", "/v2/dagger/engine/tags/list", "/v2/{repo}/tags/list"},
		{"unknown", "/healthz", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := metricPath(tc.path); got != tc.want {
				t.Fatalf("metricPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestCustomHandlerMetrics(t *testing.T) {
	handler := NewCustomHandler(&sync.WaitGroup{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v2/dagger/engine/manifests/main", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	path := "/v2/{repo}/manifests/{tagOrDigest}"
	assertCounterValue(t, handler.requests.WithLabelValues(http.MethodGet, path), 1)
	assertCounterValue(t, handler.responses.WithLabelValues(http.MethodGet, path, "202"), 1)
}

func TestRateLimitedIPsHandler(t *testing.T) {
	tracker := redirect.NewRateLimitedIPTracker(50)
	tracker.Record("203.0.113.10")
	tracker.Record("203.0.113.10")
	tracker.Record("198.51.100.4")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rate-limited-ips", nil)

	rateLimitedIPsHandler(tracker).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var got []redirect.RateLimitedIPCount
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := []redirect.RateLimitedIPCount{
		{IP: "203.0.113.10", Count: 2},
		{IP: "198.51.100.4", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}

func TestRateLimitedIPsHandlerRejectsNonGET(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rate-limited-ips", nil)

	rateLimitedIPsHandler(redirect.NewRateLimitedIPTracker(50)).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want %q", got, http.MethodGet)
	}
}

func TestSplitIPRateLimitConfigPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
	}{
		{"single", "a.json", []string{"a.json"}},
		{"two", "a.json,b.json", []string{"a.json", "b.json"}},
		{"whitespace and trailing comma", " a.json , b.json ,", []string{"a.json", "b.json"}},
		{"empty disables", "", nil},
		{"only commas", ",,", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitIPRateLimitConfigPaths(tc.value); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitIPRateLimitConfigPaths(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

func writeRateLimitConfig(t *testing.T, name string, rpm, burst int, ranges string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	body := fmt.Sprintf(`{"rate_limit":{"requests_per_minute":%d,"burst":%d},"ip_ranges":[%s]}`, rpm, burst, ranges)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadIPRateLimitOverridesKeepsFileOrder(t *testing.T) {
	first := writeRateLimitConfig(t, "first.json", 480, 960, `"203.0.113.0/24"`)
	second := writeRateLimitConfig(t, "second.json", 120, 240, `"198.51.100.0/24"`)

	overrides, err := loadIPRateLimitOverrides([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 2 {
		t.Fatalf("overrides = %d, want 2", len(overrides))
	}
	// Order is precedence: the limiter applies the first matching override.
	if overrides[0].RequestsPerMinute != 480 || overrides[1].RequestsPerMinute != 120 {
		t.Fatalf("override order = [%d, %d], want [480, 120]",
			overrides[0].RequestsPerMinute, overrides[1].RequestsPerMinute)
	}
}

func TestLoadIPRateLimitOverridesMissingFileIsNotExist(t *testing.T) {
	first := writeRateLimitConfig(t, "first.json", 480, 960, `"203.0.113.0/24"`)
	missing := filepath.Join(t.TempDir(), "missing.json")

	_, err := loadIPRateLimitOverrides([]string{first, missing})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist so redirectOptions can distinguish an unset flag", err)
	}
}

func assertCounterValue(t *testing.T, metric prometheusMetric, want float64) {
	t.Helper()

	var got dto.Metric
	if err := metric.Write(&got); err != nil {
		t.Fatalf("writing metric: %v", err)
	}
	if got.Counter == nil {
		t.Fatal("metric is not a counter")
	}
	if got.Counter.GetValue() != want {
		t.Fatalf("counter = %v, want %v", got.Counter.GetValue(), want)
	}
}

type prometheusMetric interface {
	Write(*dto.Metric) error
}
