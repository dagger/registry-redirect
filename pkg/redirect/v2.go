package redirect

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"knative.dev/pkg/logging"
)

const (
	v2CacheTTL          = time.Minute
	v2CacheMaxBodyBytes = 64 * 1024
)

func (rdr *redirect) v2(resp http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := logging.FromContext(ctx)

	// Upstream discovery is always anonymous. Cache GET responses and rewrite
	// their realm for each caller; other methods still go upstream.
	if req.Method == http.MethodGet {
		if entry, ok := rdr.v2Cache.get("/v2/"); ok {
			cacheHits.WithLabelValues(rdr.host, "v2", req.Method).Inc()
			logger.Infow("serving cached v2 challenge",
				"url", req.URL.String(),
				"status", fmt.Sprintf("%d %s", entry.status, http.StatusText(entry.status)))
			resp.Header().Set("X-Redirected", req.URL.String())
			rdr.writeV2Headers(resp.Header(), entry.header, req.Host)
			resp.WriteHeader(entry.status)
			_, _ = resp.Write(entry.body)
			return
		}
	}

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

	if !rdr.rateLimiter.allowRequest(req) {
		writeRateLimited(resp)
		return
	}

	logger.Infow("sending request",
		"method", req.Method,
		"url", req.URL.String(),
		"header", redact(req.Header))
	resp.Header().Set("X-Redirected", req.URL.String())

	back, err := rdr.doBackendRequest(rdr.client, "v2", out)
	if err != nil {
		writeBackendError(ctx, resp, "sending request", err)
		return
	}
	defer back.Body.Close()

	logger.Infow("got response",
		"method", req.Method,
		"url", req.URL.String(),
		"status", back.Status,
		"header", redact(back.Header))

	rdr.writeV2Headers(resp.Header(), back.Header, req.Host)

	if req.Method == http.MethodGet && (back.StatusCode == http.StatusOK || back.StatusCode == http.StatusUnauthorized) {
		body, tooLarge, err := readBodyWithLimit(back.Body, v2CacheMaxBodyBytes)
		if err != nil {
			logger.Errorf("Error reading response body: %v", err)
			http.Error(resp, err.Error(), http.StatusInternalServerError)
			return
		}
		if !tooLarge {
			rdr.v2Cache.set("/v2/", newCachedResponse(back.StatusCode, back.Header, body))
		}
		resp.WriteHeader(back.StatusCode)
		if _, err := resp.Write(body); err != nil {
			return
		}
		if tooLarge {
			_, _ = io.Copy(resp, back.Body)
		}
		return
	}

	resp.WriteHeader(back.StatusCode)
	if _, err := io.Copy(resp, back.Body); err != nil {
		logger.Errorf("Error copying response body: %v", err)
	}
}

// writeV2Headers rewrites the challenge so clients fetch tokens from this proxy.
func (rdr *redirect) writeV2Headers(dst, src http.Header, host string) {
	for k, v := range src {
		for _, vv := range v {
			if k == "Www-Authenticate" {
				if rdr.host == "gcr.io" {
					// GCR's token endpoint is /v2/token; callers should hit us at /token.
					vv = strings.Replace(vv, `realm="https://gcr.io/v2/`, fmt.Sprintf(`realm="https://%s/`, host), 1)
				} else {
					vv = strings.Replace(vv, `realm="https://ghcr.io/`, fmt.Sprintf(`realm="https://%s/`, host), 1)
				}
			}
			dst.Add(k, vv)
		}
	}
}
