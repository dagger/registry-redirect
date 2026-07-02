package redirect

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dgraph-io/ristretto"
)

const (
	blobRedirectCacheExpiryMargin = time.Minute
	blobRedirectCacheMaxTTL       = 5 * time.Minute
)

type blobRedirectCache struct {
	cache *ristretto.Cache
}

type blobRedirectCacheEntry struct {
	status int
	header http.Header
}

func newBlobRedirectCache(maxBytes int64) (*blobRedirectCache, error) {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: blobRedirectCacheNumCounters(maxBytes),
		MaxCost:     maxBytes,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}
	return &blobRedirectCache{cache: cache}, nil
}

func blobRedirectCacheNumCounters(maxBytes int64) int64 {
	counters := (maxBytes / 1024) * 10
	if counters < 10000 {
		return 10000
	}
	if counters > 10000000 {
		return 10000000
	}
	return counters
}

func (c *blobRedirectCache) get(key string) (*blobRedirectCacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.cache.Get(key)
	if !ok {
		return nil, false
	}
	entry, ok := value.(*blobRedirectCacheEntry)
	if !ok {
		return nil, false
	}
	return &blobRedirectCacheEntry{
		status: entry.status,
		header: entry.header.Clone(),
	}, true
}

func (c *blobRedirectCache) set(key string, resp *http.Response, now time.Time) bool {
	if c == nil || !isBlobRedirectCacheableStatus(resp.StatusCode) {
		return false
	}
	ttl, ok := blobRedirectCacheTTL(resp.Header.Get("Location"), now)
	if !ok {
		return false
	}

	entry := &blobRedirectCacheEntry{
		status: resp.StatusCode,
		header: resp.Header.Clone(),
	}
	if !c.cache.SetWithTTL(key, entry, blobRedirectCacheEntrySize(key, entry), ttl) {
		return false
	}
	c.cache.Wait()
	return true
}

func isBlobRedirectCacheableStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func blobRedirectCacheTTL(location string, now time.Time) (time.Duration, bool) {
	if location == "" {
		return 0, false
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return 0, false
	}
	expiresAt, ok := blobRedirectURLExpiry(parsed)
	if !ok {
		return 0, false
	}

	ttl := expiresAt.Sub(now) - blobRedirectCacheExpiryMargin
	if ttl <= 0 {
		return 0, false
	}
	if ttl > blobRedirectCacheMaxTTL {
		ttl = blobRedirectCacheMaxTTL
	}
	return ttl, true
}

func blobRedirectURLExpiry(u *url.URL) (time.Time, bool) {
	value := u.Query().Get("se")
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func blobRedirectCacheEntrySize(key string, entry *blobRedirectCacheEntry) int64 {
	size := len(key) + len(strconv.Itoa(entry.status))
	for header, values := range entry.header {
		size += len(header)
		for _, value := range values {
			size += len(value) + 4
		}
	}
	return int64(size)
}
