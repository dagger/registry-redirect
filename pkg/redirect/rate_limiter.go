package redirect

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ipRateLimiter struct {
	mu       sync.Mutex
	opts     RateLimitOptions
	now      func() time.Time
	limit    rate.Limit
	clients  map[string]*ipLimiter
	lastTrim time.Time
}

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(opts RateLimitOptions) *ipRateLimiter {
	return &ipRateLimiter{
		opts:    opts,
		now:     time.Now,
		limit:   rate.Limit(float64(opts.RequestsPerMinute) / 60),
		clients: map[string]*ipLimiter{},
	}
}

func (l *ipRateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBlobRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !l.allow(clientIP(r)) {
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
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
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
		client = &ipLimiter{
			limiter:  rate.NewLimiter(l.limit, l.opts.Burst),
			lastSeen: now,
		}
		l.clients[ip] = client
	}

	client.lastSeen = now
	return client.limiter.AllowN(now, 1)
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
