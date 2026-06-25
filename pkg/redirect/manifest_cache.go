package redirect

import (
	"container/list"
	"net/http"
	"sync"
)

type manifestCache struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	items    map[string]*list.Element
	order    *list.List
}

type manifestCacheEntry struct {
	key    string
	status int
	header http.Header
	body   []byte
	size   int64
}

func newManifestCache(maxBytes int64) *manifestCache {
	return &manifestCache{
		maxBytes: maxBytes,
		items:    map[string]*list.Element{},
		order:    list.New(),
	}
}

func (c *manifestCache) maxBodyBytes() int64 {
	return c.maxBytes
}

func (c *manifestCache) get(key string) (*manifestCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el := c.items[key]
	if el == nil {
		return nil, false
	}
	c.order.MoveToFront(el)
	entry := el.Value.(*manifestCacheEntry)
	return &manifestCacheEntry{
		key:    entry.key,
		status: entry.status,
		header: entry.header.Clone(),
		body:   entry.body,
		size:   entry.size,
	}, true
}

func (c *manifestCache) set(key string, status int, header http.Header, body []byte) bool {
	size := manifestCacheEntrySize(header, body)
	if size > c.maxBytes {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing := c.items[key]; existing != nil {
		entry := existing.Value.(*manifestCacheEntry)
		c.used -= entry.size
		c.order.Remove(existing)
		delete(c.items, key)
	}

	entry := &manifestCacheEntry{
		key:    key,
		status: status,
		header: header.Clone(),
		body:   body,
		size:   size,
	}
	c.items[key] = c.order.PushFront(entry)
	c.used += size

	for c.used > c.maxBytes {
		c.removeOldest()
	}
	return true
}

func (c *manifestCache) removeOldest() {
	oldest := c.order.Back()
	if oldest == nil {
		return
	}
	entry := oldest.Value.(*manifestCacheEntry)
	delete(c.items, entry.key)
	c.used -= entry.size
	c.order.Remove(oldest)
}

func manifestCacheEntrySize(header http.Header, body []byte) int64 {
	size := len(body)
	for key, values := range header {
		size += len(key)
		for _, value := range values {
			size += len(value) + 4
		}
	}
	return int64(size)
}
