/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/registry-redirect/pkg/logger"
	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
	"github.com/prometheus/client_golang/prometheus"
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

	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "registry-redirect"
	}

	// Minimum severity of the messages that the logger will log:
	// info, warning, error and fatal
	logCfg := logger.Config{
		Level:     "info",
		Component: appName,
		Protocol:  "tcp",
	}

	ctx, err := logger.NewLogger(ctx, &logCfg)
	if err != nil {
		panic(err)
	}

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
		Addr:        fmt.Sprintf(":%s", port),
		Handler:     customHandler,
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}
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

	// Wait for in-flight requests to complete before shutting down
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

	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

func NewCustomHandler(wg *sync.WaitGroup, handler http.Handler) *CustomHandler {
	ch := &CustomHandler{wg: wg, handler: handler}
	ch.requests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "requests_total",
			Help:        "Number of HTTP requests partitioned by method and HTTP path.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"method", "path"})

	ch.latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "request_duration_ms",
		Help:        "Time spent on the request partitioned by method and HTTP path.",
		ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		Buckets:     []float64{50, 150, 300, 500, 1200, 5000, 10000},
	}, []string{"method", "path"})
	return ch
}

var (
	serverPaths = []string{
		"/v2/dagger/engine/blobs",
		"/v2/engine/blobs",
		"/v2/engine/manifests",
		"/token",
	}
)

func (h *CustomHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	h.wg.Add(1)
	defer h.wg.Done()

	path := "unknown"
	for _, p := range serverPaths {
		if strings.Contains(r.URL.Path, p) {
			path = p
			break
		}
	}

	defer func() {
		h.requests.WithLabelValues(r.Method, path).Inc()
		h.latency.WithLabelValues(r.Method, path).Observe(float64(time.Since(start).Milliseconds()))
	}()

	h.handler.ServeHTTP(w, r) // Call your original handler
}
