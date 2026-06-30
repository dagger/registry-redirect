package redirect

import (
	"sort"
	"sync"
)

const defaultRateLimitedIPTrackerLimit = 50

type RateLimitedIPTracker struct {
	mu       sync.Mutex
	limit    int
	sequence uint64
	counts   map[string]*rateLimitedIPEntry
}

type RateLimitedIPCount struct {
	IP    string `json:"ip"`
	Count uint64 `json:"count"`
}

type rateLimitedIPEntry struct {
	ip       string
	count    uint64
	sequence uint64
}

func NewRateLimitedIPTracker(limit int) *RateLimitedIPTracker {
	if limit <= 0 {
		limit = defaultRateLimitedIPTrackerLimit
	}
	return &RateLimitedIPTracker{
		limit:  limit,
		counts: map[string]*rateLimitedIPEntry{},
	}
}

func (t *RateLimitedIPTracker) Record(ip string) {
	if t == nil || ip == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.sequence++
	if entry := t.counts[ip]; entry != nil {
		entry.count++
		entry.sequence = t.sequence
		return
	}

	if len(t.counts) >= t.limit {
		t.removeLowestLocked()
	}
	t.counts[ip] = &rateLimitedIPEntry{
		ip:       ip,
		count:    1,
		sequence: t.sequence,
	}
}

func (t *RateLimitedIPTracker) Top() []RateLimitedIPCount {
	if t == nil {
		return nil
	}

	counts, limit := t.snapshot()
	sortRateLimitedIPCounts(counts)
	if len(counts) > limit {
		counts = counts[:limit]
	}
	return counts
}

func (t *RateLimitedIPTracker) snapshot() ([]RateLimitedIPCount, int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	counts := make([]RateLimitedIPCount, 0, len(t.counts))
	for _, entry := range t.counts {
		counts = append(counts, RateLimitedIPCount{
			IP:    entry.ip,
			Count: entry.count,
		})
	}
	return counts, t.limit
}

func (t *RateLimitedIPTracker) TopMap() map[string]uint64 {
	top := t.Top()
	out := make(map[string]uint64, len(top))
	for _, entry := range top {
		out[entry.IP] = entry.Count
	}
	return out
}

func sortRateLimitedIPCounts(counts []RateLimitedIPCount) {
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}
		return counts[i].IP < counts[j].IP
	})
}

func (t *RateLimitedIPTracker) removeLowestLocked() {
	var lowest *rateLimitedIPEntry
	for _, entry := range t.counts {
		if lowest == nil || lessNoisy(entry, lowest) {
			lowest = entry
		}
	}
	if lowest != nil {
		delete(t.counts, lowest.ip)
	}
}

func lessNoisy(a, b *rateLimitedIPEntry) bool {
	if a.count != b.count {
		return a.count < b.count
	}
	return a.sequence < b.sequence
}
