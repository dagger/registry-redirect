package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
