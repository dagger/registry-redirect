package redirect

import (
	"net/http"
	"sync"
	"time"
)

// responseCache is a mutex-guarded map with a fixed TTL and an entry cap. It backs
// the anonymous token and /v2/ response caches, with immediately visible writes.
type responseCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]responseCacheEntry
}

type responseCacheEntry struct {
	value     *cachedResponse
	expiresAt time.Time
}

func newResponseCache(ttl time.Duration, maxEntries int) *responseCache {
	return &responseCache{
		ttl:     ttl,
		max:     maxEntries,
		entries: map[string]responseCacheEntry{},
	}
}

// get returns the value for key if it has not expired. An expired entry is
// removed on the way out.
func (c *responseCache) get(key string) (*cachedResponse, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.value, true
}

// set stores value under key for the cache's TTL. When the cache is full it
// first drops expired entries; if it is still full the value is not stored
// and the caller serves it uncached. It reports whether it was stored.
func (c *responseCache) set(key string, value *cachedResponse) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		for k, e := range c.entries {
			if !now.Before(e.expiresAt) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			return false
		}
	}
	c.entries[key] = responseCacheEntry{value: value, expiresAt: now.Add(c.ttl)}
	return true
}

// cachedResponse is immutable after construction. Callers may read its
// headers and body, but must copy headers before rewriting them for a client.
type cachedResponse struct {
	status int
	header http.Header
	body   []byte
}

func newCachedResponse(status int, header http.Header, body []byte) *cachedResponse {
	return &cachedResponse{status: status, header: header.Clone(), body: append([]byte(nil), body...)}
}
