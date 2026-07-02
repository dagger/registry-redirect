/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package redirect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"knative.dev/pkg/logging"
)

var prefixlessHosts = map[string]bool{
	"registry.dagger.io": true,
}

var (
	backendRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "registry_backend_requests_total",
			Help:        "Number of outbound registry backend HTTP requests partitioned by registry, operation, and method.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"registry", "operation", "method"})

	backendResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "registry_backend_responses_total",
			Help:        "Number of outbound registry backend HTTP responses partitioned by registry, operation, method, and status code.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"registry", "operation", "method", "status"})

	backendErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "registry_backend_errors_total",
			Help:        "Number of outbound registry backend HTTP request errors partitioned by registry, operation, method, and error class.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"registry", "operation", "method", "error"})

	backendLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "registry_backend_request_duration_ms",
			Help:        "Time spent waiting for outbound registry backend HTTP response headers partitioned by registry, operation, and method.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
			Buckets:     []float64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		}, []string{"registry", "operation", "method"})

	backendInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "registry_backend_in_flight_requests",
			Help:        "Number of in-flight outbound registry backend HTTP requests partitioned by registry, operation, and method.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"registry", "operation", "method"})

	cacheHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "registry_cache_hits_total",
			Help:        "Number of registry redirect cache hits partitioned by registry, operation, and method.",
			ConstLabels: prometheus.Labels{"service": "registry-redirect"},
		}, []string{"registry", "operation", "method"})
)

func init() {
	prometheus.MustRegister(backendRequests, backendResponses, backendErrors, backendLatency, backendInFlight, cacheHits)
}

func redact(in http.Header) http.Header {
	h := in.Clone()
	if h.Get("Authorization") != "" {
		h.Set("Authorization", "REDACTED")
	}
	return h
}

func New(host, repo, prefix string) http.Handler {
	return NewWithOptions(host, repo, prefix, DefaultOptions())
}

func NewWithOptions(host, repo, prefix string, opts Options) http.Handler {
	opts = opts.withDefaults()
	rdr := redirect{
		host:        host,
		repo:        repo,
		prefix:      prefix,
		client:      opts.Client,
		proxyClient: noRedirectClient(opts.Client, opts.Transport),
	}
	if !opts.RateLimit.Disabled {
		rdr.rateLimiter = newIPRateLimiter(opts.RateLimit)
	}
	if !opts.ManifestCache.Disabled {
		rdr.manifestCache = newManifestCache(opts.ManifestCache.MaxBytes)
	}
	if !opts.BlobCache.Disabled {
		blobCache, err := newBlobRedirectCache(opts.BlobCache.MaxBytes)
		if err != nil {
			panic(fmt.Sprintf("creating blob redirect cache: %v", err))
		}
		rdr.blobCache = blobCache
	}
	router := mux.NewRouter()

	router.Handle("/", http.RedirectHandler("https://github.com/dagger/dagger", http.StatusTemporaryRedirect))

	router.HandleFunc("/v2", rdr.v2)
	router.HandleFunc("/v2/", rdr.v2)

	router.HandleFunc("/token", rdr.token)

	router.HandleFunc("/v2/{repo:.*}/manifests/{tagOrDigest:.*}", rdr.proxy)
	router.HandleFunc("/v2/{repo:.*}/blobs/{digest:.*}", rdr.proxy)
	router.HandleFunc("/v2/{repo:.*}/tags/list", rdr.proxy)

	router.NotFoundHandler = http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		logger := logging.FromContext(ctx)
		logger.Infow("got request",
			"method", req.Method,
			"url", req.URL.String(),
			"header", redact(req.Header))
		resp.WriteHeader(http.StatusNotFound)
	})

	if rdr.rateLimiter == nil {
		return router
	}
	return rdr.rateLimiter.wrap(router)
}

type redirect struct {
	host          string
	repo          string
	prefix        string
	client        *http.Client
	proxyClient   *http.Client
	rateLimiter   *ipRateLimiter
	manifestCache *manifestCache
	blobCache     *blobRedirectCache
}

func noRedirectClient(client *http.Client, transport http.RoundTripper) *http.Client {
	out := *client
	out.Transport = transport
	out.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &out
}

func newBackendRequest(ctx context.Context, method, url string, header http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if header != nil {
		req.Header = header.Clone()
	}
	return req, nil
}

func backendErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

func backendErrorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return "timeout"
	}
	return "other"
}

func backendOperation(path string) string {
	switch path {
	case "/v2", "/v2/":
		return "v2"
	case "/token", "/v2/token":
		return "token"
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "other"
	}
	if len(parts) >= 4 && parts[len(parts)-2] == "tags" && parts[len(parts)-1] == "list" {
		return "tags"
	}
	for i := 1; i < len(parts)-1; i++ {
		switch parts[i] {
		case "manifests":
			return "manifests"
		case "blobs":
			return "blobs"
		}
	}
	return "other"
}

func (rdr redirect) doBackendRequest(client *http.Client, operation string, req *http.Request) (*http.Response, error) {
	method := req.Method
	backendRequests.WithLabelValues(rdr.host, operation, method).Inc()
	backendInFlight.WithLabelValues(rdr.host, operation, method).Inc()

	start := time.Now()
	resp, err := client.Do(req)
	backendLatency.WithLabelValues(rdr.host, operation, method).Observe(float64(time.Since(start).Milliseconds()))
	backendInFlight.WithLabelValues(rdr.host, operation, method).Dec()
	if err != nil {
		backendErrors.WithLabelValues(rdr.host, operation, method, backendErrorClass(err)).Inc()
		return nil, err
	}

	backendResponses.WithLabelValues(rdr.host, operation, method, strconv.Itoa(resp.StatusCode)).Inc()
	return resp, nil
}

func (rdr redirect) v2(resp http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	var url string
	if rdr.host == "gcr.io" {
		url = "https://gcr.io/v2/"
	} else {
		url = "https://ghcr.io/v2/"
	}
	out, err := newBackendRequest(ctx, req.Method, url, nil)
	if err != nil {
		logger.Errorf("Error creating request: %v", err)
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Infow("sending request",
		"method", req.Method,
		"url", req.URL.String(),
		"header", redact(req.Header))
	resp.Header().Set("X-Redirected", req.URL.String())

	back, err := rdr.doBackendRequest(rdr.client, "v2", out)
	if err != nil {
		logger.Errorf("Error sending request: %v", err)
		http.Error(resp, err.Error(), backendErrorStatus(err))
		return
	}
	defer back.Body.Close()

	logger.Infow("got response",
		"method", req.Method,
		"url", req.URL.String(),
		"status", back.Status,
		"header", redact(back.Header))

	for k, v := range back.Header {
		for _, vv := range v {
			if k == "Www-Authenticate" {
				log.Println("=== BEFORE: Www-Authenticate:", vv)
				if rdr.host == "gcr.io" {
					// GCR's token endpoint is /v2/token, we want callers to hit us at /token.
					vv = strings.Replace(vv, `realm="https://gcr.io/v2/`, fmt.Sprintf(`realm="https://%s/`, req.Host), 1)
				} else {
					vv = strings.Replace(vv, `realm="https://ghcr.io/`, fmt.Sprintf(`realm="https://%s/`, req.Host), 1)
				}
				log.Println("=== CHANGED: Www-Authenticate:", vv)
			}
			resp.Header().Add(k, vv)
		}
	}
	resp.WriteHeader(back.StatusCode)
	if _, err := io.Copy(resp, back.Body); err != nil {
		logger.Errorf("Error copying response body: %v", err)
	}
}

func (rdr redirect) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromContext(ctx)

	vals := r.URL.Query()
	if rdr.prefix != "" {
		scope := vals.Get("scope")
		scope = strings.Replace(scope, rdr.prefix+"/", "", 1)
		vals.Set("scope", scope)
	}
	if rdr.repo != "" {
		scope := vals.Get("scope")
		scope = strings.Replace(scope, "repository:", "repository:"+rdr.repo+"/", 1)
		vals.Set("scope", scope)
	}

	var url string
	if rdr.host == "gcr.io" {
		url = "https://gcr.io/v2/token?" + vals.Encode()
	} else {
		url = "https://ghcr.io/token?" + vals.Encode()
	}

	req, err := newBackendRequest(ctx, r.Method, url, r.Header)
	if err != nil {
		logger.Errorf("Error creating request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Infow("sending request",
		"method", req.Method,
		"url", req.URL.String(),
		"header", redact(req.Header))
	w.Header().Set("X-Redirected", req.URL.String())

	resp, err := rdr.doBackendRequest(rdr.client, "token", req)
	if err != nil {
		logger.Errorf("Error sending request: %v", err)
		http.Error(w, err.Error(), backendErrorStatus(err))
		return
	}
	defer resp.Body.Close()

	logger.Infow("got response",
		"method", req.Method,
		"url", req.URL.String(),
		"status", resp.Status,
		"header", redact(resp.Header))

	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Errorf("Error copying response body: %v", err)
	}
}

func (rdr redirect) proxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromContext(ctx)

	var url string
	if rdr.host == "gcr.io" {
		url = "https://gcr.io/v2/"
	} else {
		url = "https://ghcr.io/v2/"
	}
	if rdr.repo != "" {
		url += rdr.repo + "/"
	}

	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	if rdr.prefix != "" && !prefixlessHosts[r.Host] {
		log.Println("=== BEFORE: path:", path)
		// Require and trim the prefix, if the request isn't coming from a prefixless host.
		if !strings.HasPrefix(path, rdr.prefix+"/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"Manifest unknown, prefix required"}]}`)
			return
		}
		path = strings.TrimPrefix(path, rdr.prefix+"/")
		log.Println("=== AFTER: path:", path)
	}

	url += path
	if query := r.URL.Query().Encode(); query != "" {
		url += "?" + query
	}
	cacheKey, isManifestCacheable := rdr.manifestCacheKey(r, url)
	if isManifestCacheable {
		if entry, ok := rdr.manifestCache.get(cacheKey); ok {
			rdr.serveCachedManifest(w, r, url, entry)
			return
		}
	}
	blobCacheKey, isBlobRedirectCacheable := rdr.blobRedirectCacheKey(r, url)
	if isBlobRedirectCacheable {
		if entry, ok := rdr.blobCache.get(blobCacheKey); ok {
			rdr.serveCachedBlobRedirect(w, r, url, entry)
			return
		}
	}

	req, err := newBackendRequest(ctx, r.Method, url, r.Header)
	if err != nil {
		logger.Errorf("Error creating request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If the request is coming in without auth, get some auth.
	// This is useful for testing, but should never happen in real life.
	// Actually, containerd seems to make unauthenticated HEAD requests before
	// hitting /v2/, so this might be load-bearing.
	if req.Header.Get("Authorization") == "" {
		logger.Warnw("request without Authorization header, getting auth")
		t, resp, err := rdr.getToken(r)
		if err != nil {
			if resp != nil {
				logger.Infof("Error response getting token: %d %s", resp.StatusCode, resp.Status)
				http.Error(w, resp.Status, resp.StatusCode)
				return
			}
			logger.Errorf("Error getting token: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}

	logger.Infow("sending request",
		"method", req.Method,
		"url", req.URL.String(),
		"header", redact(req.Header))
	w.Header().Set("X-Redirected", req.URL.String())

	resp, err := rdr.doBackendRequest(rdr.proxyClient, backendOperation(req.URL.Path), req)
	if err != nil {
		logger.Errorf("Error sending request: %v", err)
		http.Error(w, err.Error(), backendErrorStatus(err))
		return
	}
	defer resp.Body.Close()

	logger.Infow("got response",
		"method", r.Method,
		"url", r.URL.String(),
		"status", resp.Status,
		"header", redact(resp.Header))

	for k, v := range resp.Header {
		for _, vv := range v {
			// List responses include a response header to support pagination, that looks like:
			//   Link: </v2/distroless/static/tags/list?n=100&last=blah>; rel="next">
			//
			// In order for the client to be able to use this link, we need to rewrite it to
			// point to the user's requested repo, not the upstream:
			//   Link: </v2[/prefix]/static/repo/tags/list?n=100&last=blah>; rel="next">
			if k == "Link" && strings.HasPrefix(vv, "</v2/"+rdr.repo) {
				log.Println("=== BEFORE: Link:", vv)
				rest := strings.TrimPrefix(vv, "</v2/"+rdr.repo)
				vv = "</v2" + rest
				if rdr.prefix != "" && !prefixlessHosts[r.Host] {
					vv = "</v2/" + rdr.prefix + rest
				}
				log.Println("=== CHANGED: Link:", vv)
			}

			w.Header().Add(k, vv)
		}
	}

	// If it's a list request, rewrite the response so the name key matches the
	// user's requested repo, otherwise clients will repeatedly request the
	// first page looking for their repo's tags.
	if rdr.repo != "" && strings.Contains(r.URL.Path, "/tags/list") {
		var lr listResponse
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			logger.Errorf("Error decoding list response body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Println("=== BEFORE: Name:", lr.Name)
		lr.Name = strings.Replace(lr.Name, rdr.repo+"/", "", 1)
		log.Println("=== CHANGED: Name:", lr.Name)

		// Unset the content-length header from our response, because we're
		// about to rewrite the response to be shorter than the original.
		// This can confuse Cloud Run, which responds with an empty body
		// if the content-length header is wrong in some cases.
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		if err := json.NewEncoder(w).Encode(lr); err != nil {
			logger.Errorf("Error encoding list response body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}

	if isManifestCacheable && r.Method == http.MethodGet && resp.StatusCode == http.StatusOK {
		rdr.proxyAndCacheManifest(w, resp, cacheKey)
		return
	}
	if isBlobRedirectCacheable {
		rdr.blobCache.set(blobCacheKey, resp, time.Now())
	}

	w.WriteHeader(resp.StatusCode)

	// Unless we're serving blobs, also proxy the response body, if any.
	// Most of the time blob responses will just be 302 redirects to
	// another location, likely a CDN, but just in case we get a "real"
	// response we'd like to avoid paying the egress cost to serve it.
	// Manifests may also be served with redirects, but if they're not,
	// they're likely small enough we don't mind paying to proxy them.
	parts := strings.Split(r.URL.Path, "/")
	if parts[len(parts)-2] != "blobs" {
		if _, err := io.Copy(w, resp.Body); err != nil {
			logger.Errorf("Error copying response body: %v", err)
		}
	}
}

func (rdr redirect) blobRedirectCacheKey(r *http.Request, upstreamURL string) (string, bool) {
	if rdr.blobCache == nil {
		return "", false
	}
	if r.Method != http.MethodGet {
		return "", false
	}
	if !strings.Contains(r.URL.Path, "/blobs/") {
		return "", false
	}
	return r.Method + "\n" + upstreamURL, true
}

func (rdr redirect) serveCachedBlobRedirect(w http.ResponseWriter, r *http.Request, upstreamURL string, entry *blobRedirectCacheEntry) {
	copyHeaders(w.Header(), entry.header)
	w.Header().Set("X-Redirected", upstreamURL)
	cacheHits.WithLabelValues(rdr.host, "blobs", r.Method).Inc()
	rdr.logCachedBlobRedirect(w, r, upstreamURL, entry.status)
	w.WriteHeader(entry.status)
}

func (rdr redirect) logCachedBlobRedirect(w http.ResponseWriter, r *http.Request, upstreamURL string, status int) {
	logger := logging.FromContext(r.Context())
	logger.Infow("serving cached blob redirect",
		"method", r.Method,
		"url", r.URL.String(),
		"request_header", redact(r.Header),
		"upstream_url", upstreamURL,
		"status", fmt.Sprintf("%d %s", status, http.StatusText(status)),
		"response_header", redact(w.Header()))
}

func (rdr redirect) manifestCacheKey(r *http.Request, upstreamURL string) (string, bool) {
	if rdr.manifestCache == nil {
		return "", false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	if !strings.Contains(r.URL.Path, "/manifests/") {
		return "", false
	}
	return upstreamURL + "\naccept:" + r.Header.Get("Accept"), true
}

func (rdr redirect) serveCachedManifest(w http.ResponseWriter, r *http.Request, upstreamURL string, entry *manifestCacheEntry) {
	copyHeaders(w.Header(), entry.header)
	w.Header().Set("X-Redirected", upstreamURL)
	cacheHits.WithLabelValues(rdr.host, "manifests", r.Method).Inc()

	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), entry.header) {
		w.Header().Del("Content-Length")
		rdr.logCachedManifest(w, r, upstreamURL, http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if w.Header().Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(entry.body)))
	}
	rdr.logCachedManifest(w, r, upstreamURL, entry.status)
	w.WriteHeader(entry.status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(entry.body)
}

func (rdr redirect) logCachedManifest(w http.ResponseWriter, r *http.Request, upstreamURL string, status int) {
	logger := logging.FromContext(r.Context())
	logger.Infow("serving cached manifest",
		"method", r.Method,
		"url", r.URL.String(),
		"request_header", redact(r.Header),
		"upstream_url", upstreamURL,
		"status", fmt.Sprintf("%d %s", status, http.StatusText(status)),
		"response_header", redact(w.Header()))
}

func (rdr redirect) proxyAndCacheManifest(w http.ResponseWriter, resp *http.Response, cacheKey string) {
	body, tooLarge, err := readBodyWithLimit(resp.Body, rdr.manifestCache.maxBodyBytes())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !tooLarge {
		rdr.manifestCache.set(cacheKey, resp.StatusCode, resp.Header, body)
	}

	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		return
	}
	if tooLarge {
		_, _ = io.Copy(w, resp.Body)
	}
}

func readBodyWithLimit(body io.Reader, maxBytes int64) ([]byte, bool, error) {
	limited := io.LimitReader(body, maxBytes+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(out)) > maxBytes {
		return out, true, nil
	}
	return out, false, nil
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func ifNoneMatchMatches(value string, header http.Header) bool {
	if value == "" {
		return false
	}

	matches := map[string]bool{}
	if etag := header.Get("Etag"); etag != "" {
		matches[etag] = true
		matches[strings.TrimPrefix(etag, "W/")] = true
	}
	if digest := header.Get("Docker-Content-Digest"); digest != "" {
		matches[digest] = true
		matches[`"`+digest+`"`] = true
	}

	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || matches[candidate] || matches[strings.TrimPrefix(candidate, "W/")] {
			return true
		}
	}
	return false
}

func (rdr redirect) getToken(r *http.Request) (string, *http.Response, error) {
	parts := strings.Split(r.URL.Path, "/")
	parts = parts[2 : len(parts)-2]
	if rdr.prefix != "" && parts[0] == rdr.prefix {
		parts = parts[1:]
	}
	if rdr.repo != "" {
		parts = append([]string{rdr.repo}, parts...)
	}
	var url string
	if rdr.host == "gcr.io" {
		url = fmt.Sprintf("https://gcr.io/v2/token?scope=repository:%s:pull&service=gcr.io", strings.Join(parts, "/"))
	} else {
		url = fmt.Sprintf("https://ghcr.io/token?scope=repository:%s:pull&service=ghcr.io", strings.Join(parts, "/"))
	}
	req, err := newBackendRequest(r.Context(), http.MethodGet, url, r.Header)
	if err != nil {
		return "", nil, err
	}

	resp, err := rdr.doBackendRequest(rdr.client, "auth_token", req) //nolint:gosec
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp, fmt.Errorf("error getting token: %v", resp.Status)
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", nil, err
	}
	return t.Token, nil, nil
}

type listResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}
