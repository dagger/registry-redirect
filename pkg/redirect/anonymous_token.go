package redirect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"knative.dev/pkg/logging"
)

const (
	// anonymousTokenTTL bounds how long an anonymous token is reused. GHCR's
	// anonymous token response carries no expires_in, so the TTL is fixed
	// and kept under the Docker token spec's 60s default.
	anonymousTokenTTL = 45 * time.Second

	// tokenCacheMaxEntries bounds memory at one entry per upstream token URL.
	tokenCacheMaxEntries = 1024

	// tokenResponseMaxBodyBytes keeps a surprising upstream answer out of the
	// cache; a real token body is a couple of kilobytes.
	tokenResponseMaxBodyBytes = 64 * 1024
)

// Preserve every query parameter, while giving equivalent client and proxy
// requests the same cache and singleflight key.
func (rdr *redirect) tokenURL(query url.Values) string {
	base := "https://ghcr.io/token"
	if rdr.host == "gcr.io" {
		base = "https://gcr.io/v2/token"
	}
	return base + "?" + query.Encode()
}

func (rdr *redirect) cachedToken(url, operation string) (*cachedResponse, bool) {
	answer, ok := rdr.tokenCache.get(url)
	if ok {
		cacheHits.WithLabelValues(rdr.host, operation, http.MethodGet).Inc()
	}
	return answer, ok
}

// fetchAnonymousToken shares one upstream fetch between anonymous callers.
// Handlers must check each caller's rate limit before joining the fetch.
func (rdr *redirect) fetchAnonymousToken(ctx context.Context, url, operation string) (*cachedResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The shared fetch survives disconnects and is bounded by Client.Timeout.
	fetchCtx := context.WithoutCancel(ctx)
	result := rdr.tokenGroup.DoChan(url, func() (any, error) {
		// A previous flight may have filled the cache since our first lookup.
		if answer, ok := rdr.tokenCache.get(url); ok {
			return answer, nil
		}
		// Every anonymous fetch requests the same representation, independent
		// of the leader's cookies, Accept-Encoding, range or conditional headers.
		req, err := newBackendRequest(fetchCtx, http.MethodGet, url, http.Header{
			"Accept":          {"application/json"},
			"Accept-Encoding": {"identity"},
		})
		if err != nil {
			return nil, err
		}
		resp, err := rdr.doBackendRequest(rdr.client, operation, req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, tooLarge, err := readBodyWithLimit(resp.Body, tokenResponseMaxBodyBytes)
		if err != nil {
			return nil, err
		}
		if tooLarge {
			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			body = append(body, rest...)
		}
		answer := newCachedResponse(resp.StatusCode, resp.Header, body)
		if resp.StatusCode == http.StatusOK && !tooLarge {
			// Relay malformed answers to /token clients, but don't let one
			// poison subsequent token lookups for either caller.
			if _, err := decodeToken(body); err == nil {
				rdr.tokenCache.set(url, answer)
			}
		}
		return answer, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-result:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*cachedResponse), nil
	}
}

func decodeToken(body []byte) (string, error) {
	var token struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&token); err != nil {
		return "", err
	}
	return token.Token, nil
}

func (rdr *redirect) token(w http.ResponseWriter, r *http.Request) {
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

	url := rdr.tokenURL(vals)

	// Only anonymous GET responses may be shared between clients.
	if r.Method == http.MethodGet && r.Header.Get("Authorization") == "" {
		rdr.serveAnonymousToken(w, r, url)
		return
	}

	req, err := newBackendRequest(ctx, r.Method, url, r.Header)
	if err != nil {
		logger.Errorf("Error creating request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !rdr.rateLimiter.allowRequest(r) {
		writeRateLimited(w)
		return
	}

	logger.Infow("sending request",
		"method", req.Method,
		"url", req.URL.String(),
		"header", redact(req.Header))
	w.Header().Set("X-Redirected", req.URL.String())

	resp, err := rdr.doBackendRequest(rdr.client, "token", req)
	if err != nil {
		writeBackendError(ctx, w, "sending request", err)
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

// serveAnonymousToken relays the same anonymous response getToken decodes.
func (rdr *redirect) serveAnonymousToken(w http.ResponseWriter, r *http.Request, url string) {
	answer, ok := rdr.cachedToken(url, "token")
	if !ok {
		if !rdr.rateLimiter.allowRequest(r) {
			writeRateLimited(w)
			return
		}
		var err error
		answer, err = rdr.fetchAnonymousToken(r.Context(), url, "token")
		if err != nil {
			writeBackendError(r.Context(), w, "sending request", err)
			return
		}
	}
	copyHeaders(w.Header(), answer.header)
	w.Header().Set("X-Redirected", url)
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body)
}

// getToken returns an anonymous pull token for the repository in r's path.
// The proxy only calls it for requests without Authorization.
func (rdr *redirect) getToken(r *http.Request) (string, error) {
	parts := strings.Split(r.URL.Path, "/")
	parts = parts[2 : len(parts)-2]
	if rdr.prefix != "" && parts[0] == rdr.prefix {
		parts = parts[1:]
	}
	if rdr.repo != "" {
		parts = append([]string{rdr.repo}, parts...)
	}
	service := "ghcr.io"
	if rdr.host == "gcr.io" {
		service = "gcr.io"
	}
	url := rdr.tokenURL(url.Values{
		"scope":   {"repository:" + strings.Join(parts, "/") + ":pull"},
		"service": {service},
	})
	answer, ok := rdr.cachedToken(url, "auth_token")
	if !ok {
		var err error
		answer, err = rdr.fetchAnonymousToken(r.Context(), url, "auth_token")
		if err != nil {
			return "", err
		}
	}
	if answer.status != http.StatusOK {
		return "", &tokenStatusError{code: answer.status, status: fmt.Sprintf("%d %s", answer.status, http.StatusText(answer.status))}
	}
	return decodeToken(answer.body)
}

// tokenStatusError carries a non-200 answer from the upstream token endpoint
// so it can be relayed to every request that shared the fetch, without
// sharing a response body between them.
type tokenStatusError struct {
	code   int
	status string
}

func (e *tokenStatusError) Error() string { return "error getting token: " + e.status }
