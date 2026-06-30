package redirect_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
)

func TestRateLimitedIPTrackerTop(t *testing.T) {
	tracker := redirect.NewRateLimitedIPTracker(4)

	for _, ip := range []string{
		"203.0.113.1",
		"203.0.113.1",
		"203.0.113.1",
		"203.0.113.1",
		"203.0.113.2",
		"203.0.113.2",
		"203.0.113.3",
		"203.0.113.3",
		"203.0.113.4",
	} {
		tracker.Record(ip)
	}

	got := tracker.Top()
	want := []redirect.RateLimitedIPCount{
		{IP: "203.0.113.1", Count: 4},
		{IP: "203.0.113.2", Count: 2},
		{IP: "203.0.113.3", Count: 2},
		{IP: "203.0.113.4", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Top() = %#v, want %#v", got, want)
	}

	gotMap := tracker.TopMap()
	wantMap := map[string]uint64{
		"203.0.113.1": 4,
		"203.0.113.2": 2,
		"203.0.113.3": 2,
		"203.0.113.4": 1,
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Fatalf("TopMap() = %#v, want %#v", gotMap, wantMap)
	}
}

func TestRateLimitedIPTrackerBoundsStoredIPs(t *testing.T) {
	tracker := redirect.NewRateLimitedIPTracker(2)

	tracker.Record("203.0.113.1")
	tracker.Record("203.0.113.1")
	tracker.Record("203.0.113.2")
	tracker.Record("203.0.113.3")

	got := tracker.Top()
	want := []redirect.RateLimitedIPCount{
		{IP: "203.0.113.1", Count: 2},
		{IP: "203.0.113.3", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Top() = %#v, want %#v", got, want)
	}
}

func TestRateLimitedIPTrackerConcurrentRecord(t *testing.T) {
	tracker := redirect.NewRateLimitedIPTracker(50)

	const (
		workers           = 8
		recordsPerWorker  = 1000
		wantRecordedCount = workers * recordsPerWorker
	)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < recordsPerWorker; j++ {
				tracker.Record("203.0.113.10")
			}
		}()
	}
	wg.Wait()

	got := tracker.TopMap()["203.0.113.10"]
	if got != wantRecordedCount {
		t.Fatalf("recorded count = %d, want %d", got, wantRecordedCount)
	}
}
