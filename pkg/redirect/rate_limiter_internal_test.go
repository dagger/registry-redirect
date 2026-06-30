package redirect

import (
	"net/netip"
	"path/filepath"
	"testing"

	"go4.org/netipx"
)

func TestIPSetContainsConfiguredBoundaries(t *testing.T) {
	ipSet := mustNewIPSet(t, []netip.Prefix{
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("198.51.100.10/32"),
		netip.MustParsePrefix("2001:db8:abcd::/48"),
		netip.MustParsePrefix("2001:db8:ffff::1/128"),
	})

	for _, ip := range []string{
		"203.0.113.0",
		"203.0.113.255",
		"198.51.100.10",
		"2001:db8:abcd::",
		"2001:db8:abcd:ffff:ffff:ffff:ffff:ffff",
		"2001:db8:ffff::1",
	} {
		if !ipSet.Contains(netip.MustParseAddr(ip)) {
			t.Fatalf("expected %s to match", ip)
		}
	}

	for _, ip := range []string{
		"203.0.112.255",
		"203.0.114.0",
		"198.51.100.9",
		"198.51.100.11",
		"2001:db8:abcc:ffff:ffff:ffff:ffff:ffff",
		"2001:db8:abce::",
		"2001:db8:ffff::",
		"2001:db8:ffff::2",
	} {
		if ipSet.Contains(netip.MustParseAddr(ip)) {
			t.Fatalf("expected %s not to match", ip)
		}
	}
}

func TestIPSetContainsIPv4MappedClientAfterUnmap(t *testing.T) {
	ipSet := mustNewIPSet(t, []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
	})

	addr := netip.MustParseAddr("::ffff:192.0.2.10").Unmap()
	if !ipSet.Contains(addr) {
		t.Fatal("expected mapped IPv4 address to match IPv4 range after unmap")
	}
}

func TestShippedGitHubActionsIPSetContainsEveryConfiguredBoundary(t *testing.T) {
	prefixes := loadShippedGitHubActionsPrefixes(t)
	ipSet := mustNewIPSet(t, prefixes)

	for _, prefix := range prefixes {
		r := netipx.RangeOfPrefix(prefix.Masked())
		if !r.IsValid() {
			t.Fatalf("invalid prefix in shipped config: %s", prefix)
		}
		if !ipSet.Contains(r.From().Unmap()) {
			t.Fatalf("expected IP set to contain start boundary %s for %s", r.From(), prefix)
		}
		if !ipSet.Contains(r.To().Unmap()) {
			t.Fatalf("expected IP set to contain end boundary %s for %s", r.To(), prefix)
		}
	}
}

func TestShippedGitHubActionsIPSetMatchesLinearPrefixChecks(t *testing.T) {
	prefixes := loadShippedGitHubActionsPrefixes(t)
	ipSet := mustNewIPSet(t, prefixes)
	samples := map[netip.Addr]struct{}{}

	for _, ip := range []string{
		"1.1.1.1",
		"8.8.8.8",
		"10.0.0.1",
		"127.0.0.1",
		"192.168.1.1",
		"2001:4860:4860::8888",
		"fd00::1",
	} {
		samples[netip.MustParseAddr(ip)] = struct{}{}
	}

	for i, prefix := range prefixes {
		if i != 0 && i != len(prefixes)-1 && i%97 != 0 {
			continue
		}
		r := netipx.RangeOfPrefix(prefix.Masked())
		if !r.IsValid() {
			t.Fatalf("invalid prefix in shipped config: %s", prefix)
		}
		samples[r.From().Unmap()] = struct{}{}
		samples[r.To().Unmap()] = struct{}{}
		if prev := r.From().Prev(); prev.IsValid() {
			samples[prev.Unmap()] = struct{}{}
		}
		if next := r.To().Next(); next.IsValid() {
			samples[next.Unmap()] = struct{}{}
		}
	}

	for addr := range samples {
		got := ipSet.Contains(addr)
		want := linearPrefixContains(prefixes, addr)
		if got != want {
			t.Fatalf("IP set match for %s = %v, want %v", addr, got, want)
		}
	}
}

func mustNewIPSet(t *testing.T, prefixes []netip.Prefix) *netipx.IPSet {
	t.Helper()

	ipSet, err := newIPSet(prefixes)
	if err != nil {
		t.Fatal(err)
	}
	if ipSet == nil {
		t.Fatal("expected non-empty IP set")
	}
	return ipSet
}

func loadShippedGitHubActionsPrefixes(t *testing.T) []netip.Prefix {
	t.Helper()

	overrides, err := LoadIPRateLimitOverrides(filepath.Join("..", "..", "github-actions-rate-limit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides = %d, want 1", len(overrides))
	}
	if len(overrides[0].IPPrefixes) == 0 {
		t.Fatal("expected shipped config to contain prefixes")
	}
	return overrides[0].IPPrefixes
}

func linearPrefixContains(prefixes []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
