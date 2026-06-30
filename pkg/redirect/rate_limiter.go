package redirect

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"go4.org/netipx"
	"golang.org/x/time/rate"
)

type ipRateLimiter struct {
	mu         sync.Mutex
	opts       RateLimitOptions
	now        func() time.Time
	limit      rate.Limit
	burst      int
	overrides  []compiledIPRateLimitOverride
	limitedIPs *RateLimitedIPTracker
	clients    map[string]*ipLimiter
	lastTrim   time.Time
}

type compiledIPRateLimitOverride struct {
	rateLimit ipRateLimit
	ipSet     *netipx.IPSet
}

type ipRateLimit struct {
	limit rate.Limit
	burst int
}

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(opts RateLimitOptions) *ipRateLimiter {
	overrides := compileIPRateLimitOverrides(opts.IPOverrides)
	opts.IPOverrides = nil

	return &ipRateLimiter{
		opts:       opts,
		now:        time.Now,
		limit:      rate.Limit(float64(opts.RequestsPerMinute) / 60),
		burst:      opts.Burst,
		overrides:  overrides,
		limitedIPs: opts.LimitedIPs,
		clients:    map[string]*ipLimiter{},
	}
}

func (l *ipRateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBlobRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if !l.allow(ip) {
			l.limitedIPs.Record(ip)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"request rate limit exceeded"}]}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isBlobRequest(r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v2" {
		return false
	}
	for i := 2; i < len(parts)-1; i++ {
		if parts[i] == "blobs" {
			return true
		}
	}
	return false
}

func (l *ipRateLimiter) allow(ip string) bool {
	now := l.now()
	client := l.client(ip, now)
	return client.AllowN(now, 1)
}

func (l *ipRateLimiter) client(ip string, now time.Time) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lastTrim.IsZero() || now.Sub(l.lastTrim) >= time.Minute {
		l.removeIdle(now)
		l.lastTrim = now
	}

	client := l.clients[ip]
	if client == nil {
		if len(l.clients) >= l.opts.MaxIPs {
			l.removeIdle(now)
		}
		for len(l.clients) >= l.opts.MaxIPs {
			l.removeOldest()
		}
		rateLimit := l.rateLimitForIP(ip)
		client = &ipLimiter{
			limiter:  rate.NewLimiter(rateLimit.limit, rateLimit.burst),
			lastSeen: now,
		}
		l.clients[ip] = client
	}

	client.lastSeen = now
	return client.limiter
}

func compileIPRateLimitOverrides(overrides []IPRateLimitOverride) []compiledIPRateLimitOverride {
	compiled := make([]compiledIPRateLimitOverride, 0, len(overrides))
	for _, override := range overrides {
		if override.RequestsPerMinute <= 0 || override.Burst <= 0 || len(override.IPPrefixes) == 0 {
			continue
		}
		ipSet, err := newIPSet(override.IPPrefixes)
		if err != nil || ipSet == nil {
			continue
		}
		compiled = append(compiled, compiledIPRateLimitOverride{
			rateLimit: ipRateLimit{
				limit: rate.Limit(float64(override.RequestsPerMinute) / 60),
				burst: override.Burst,
			},
			ipSet: ipSet,
		})
	}
	return compiled
}

func (l *ipRateLimiter) rateLimitForIP(ip string) ipRateLimit {
	defaultRateLimit := ipRateLimit{
		limit: l.limit,
		burst: l.burst,
	}

	if len(l.overrides) == 0 {
		return defaultRateLimit
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return defaultRateLimit
	}
	addr = addr.Unmap()

	for _, override := range l.overrides {
		if override.ipSet.Contains(addr) {
			return override.rateLimit
		}
	}
	return defaultRateLimit
}

func newIPSet(prefixes []netip.Prefix) (*netipx.IPSet, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}

	var builder netipx.IPSetBuilder
	valid := 0
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			continue
		}
		builder.AddPrefix(prefix.Masked())
		valid++
	}
	if valid == 0 {
		return nil, nil
	}
	ipSet, err := builder.IPSet()
	if err != nil {
		return nil, err
	}
	if len(ipSet.Ranges()) == 0 {
		return nil, nil
	}
	return ipSet, nil
}

func (l *ipRateLimiter) removeIdle(now time.Time) {
	for ip, client := range l.clients {
		if now.Sub(client.lastSeen) >= l.opts.IdleTTL {
			delete(l.clients, ip)
		}
	}
}

func (l *ipRateLimiter) removeOldest() {
	var oldestIP string
	var oldest time.Time
	for ip, client := range l.clients {
		if oldestIP == "" || client.lastSeen.Before(oldest) {
			oldestIP = ip
			oldest = client.lastSeen
		}
	}
	if oldestIP != "" {
		delete(l.clients, oldestIP)
	}
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Fly-Client-IP")); ip != "" {
		return ip
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if ip := strings.TrimSpace(strings.Split(forwarded, ",")[0]); ip != "" {
			return ip
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}
