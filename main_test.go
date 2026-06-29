package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
