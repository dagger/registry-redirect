/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"knative.dev/pkg/logging"
)

// TODO:
// - Also support anonymous and Basic-type auth?
// - take a config for registries/repos to redirect from/to.

var (
	// Redirect requests for example.dev/static -> ghcr.io/static
	// If repo is empty, example.dev/foo/bar -> ghcr.io/foo/bar
	repo = flag.String("repo", "", "repo to redirect to")

	// TODO(jason): Support arbitrary registries.
	gcr = flag.Bool("gcr", false, "if true, use GCR mode")

	// prefix is the user-visible repo prefix.
	// For example, if repo is "example" and prefix is "unicorns",
	// users hitting example.dev/unicorns/foo/bar will be redirected to
	// ghcr.io/example/foo/bar.
	// If prefix is unset, hitting example.dev/unicorns/foo/bar will
	// redirect to ghcr.io/unicorns/foo/bar.
	// If prefix is set, and users hit a path without the prefix, it's ignored:
	// - example.dev/foo/bar -> ghcr.io/distroless/foo/bar
	// (this is for backward compatibility with prefix-less redirects)
	prefix = flag.String("prefix", "", "if set, user-visible repo prefix")
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())

	logger := logging.FromContext(ctx)

	go func() {
		oscall := <-c
		logger.Infof("system call:%+v", oscall)
		cancel()
	}()

	if err := serve(ctx, logger); err != nil {
		logger.Fatalf("failed to serve:+%v\n", err)
	}
}

func serve(ctx context.Context, logger *zap.SugaredLogger) (err error) {
	flag.Parse()
	host := "ghcr.io"
	if *gcr {
		host = "gcr.io"
	}
	wg := &sync.WaitGroup{}
	r := redirect.New(host, *repo, *prefix)
	customHandler := NewCustomHandler(wg, r)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Info("http server starting...")
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: customHandler,
	}
	go func() {
		// start a prometheus metric server at port 9090. Don't really care
		// for graceful termination here so we schedule it on a fire-and-forget
		// goroutine
		r := http.NewServeMux()

		// register custom handler metrics
		prometheus.DefaultRegisterer.MustRegister(customHandler.requests, customHandler.responses, customHandler.latency)

		r.Handle("/metrics", promhttp.Handler())
		if err = http.ListenAndServe(":9090", r); err != nil {
			logger.Warnf("metric server exited: %s", err)
		}
	}()
	go func() {
		if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("listen:%+s\n", err)
		}
	}()
	logger.Infof("http server listening on port: %s", port)
	<-ctx.Done()
	logger.Info("http server stopped")

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	if err = srv.Shutdown(ctxShutDown); err != nil {
		logger.Fatalf("http server shutdown failed:%+s", err)
	}

	// Wait for in-flight requests to complete before shutting down.
	wg.Wait()

	logger.Infof("http server shutdown gracefully")

	if err == http.ErrServerClosed {
		err = nil
	}

	return
}

type CustomHandler struct {
	wg      *sync.WaitGroup
	handler http.Handler

	requests  *prometheus.CounterVec
	responses *prometheus.CounterVec
	latency   *prometheus.HistogramVec
}

func NewCustomHandler(wg *sync.WaitGroup, handler http.Handler) *CustomHandler {
	ch := &CustomHandler{wg: wg, handler: handler}
	ch.requests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "requests_total",
			Help:        "Number of HTTP requests partitioned by method and HTTP path.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"method", "path"})

	ch.responses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "responses_total",
			Help:        "Number of HTTP responses partitioned by method, HTTP path, and status code.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"method", "path", "status"})

	ch.latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "request_duration_ms",
		Help:        "Time spent on the request partitioned by method and HTTP path.",
		ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		Buckets:     []float64{50, 150, 300, 500, 1200, 5000, 10000},
	}, []string{"method", "path"})
	return ch
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func metricPath(path string) string {
	switch path {
	case "/":
		return "/"
	case "/v2", "/v2/":
		return "/v2"
	case "/token":
		return "/token"
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v2" {
		return "unknown"
	}

	if len(parts) >= 5 && parts[len(parts)-2] == "tags" && parts[len(parts)-1] == "list" {
		return "/v2/{repo}/tags/list"
	}
	if hasRegistryPathSeparator(parts, "manifests") {
		return "/v2/{repo}/manifests/{tagOrDigest}"
	}
	if hasRegistryPathSeparator(parts, "blobs") {
		return "/v2/{repo}/blobs/{digest}"
	}

	return "unknown"
}

func hasRegistryPathSeparator(parts []string, separator string) bool {
	for i := len(parts) - 2; i >= 2; i-- {
		if parts[i] == separator {
			return true
		}
	}
	return false
}

func (h *CustomHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	h.wg.Add(1)
	defer h.wg.Done()

	path := metricPath(r.URL.Path)
	h.requests.WithLabelValues(r.Method, path).Inc()

	rw := &statusResponseWriter{ResponseWriter: w}

	defer func() {
		h.responses.WithLabelValues(r.Method, path, strconv.Itoa(rw.statusCode())).Inc()
		h.latency.WithLabelValues(r.Method, path).Observe(float64(time.Since(start).Milliseconds()))
	}()

	h.handler.ServeHTTP(rw, r) // Call your original handler
}
