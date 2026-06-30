package redirect_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/chainguard-dev/registry-redirect/pkg/redirect"
)

func TestLoadIPRateLimitOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate-limit.json")
	if err := os.WriteFile(path, []byte(`{
		"rate_limit": {
			"requests_per_minute": 240,
			"burst": 480
		},
		"ip_ranges": [
			"203.0.113.0/24",
			"2001:db8::1"
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	overrides, err := redirect.LoadIPRateLimitOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides = %d, want 1", len(overrides))
	}

	override := overrides[0]
	if override.RequestsPerMinute != 240 {
		t.Fatalf("requests per minute = %d, want 240", override.RequestsPerMinute)
	}
	if override.Burst != 480 {
		t.Fatalf("burst = %d, want 480", override.Burst)
	}
	if len(override.IPPrefixes) != 2 {
		t.Fatalf("IP prefixes = %d, want 2", len(override.IPPrefixes))
	}
	if !override.IPPrefixes[0].Contains(netip.MustParseAddr("203.0.113.10")) {
		t.Fatalf("first prefix does not contain configured IPv4 range")
	}
	if !override.IPPrefixes[1].Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Fatalf("second prefix does not contain configured IPv6 address")
	}
}

func TestLoadIPRateLimitOverridesRejectsInvalidRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate-limit.json")
	if err := os.WriteFile(path, []byte(`{
		"rate_limit": {
			"requests_per_minute": 240,
			"burst": 480
		},
		"ip_ranges": [
			"not-an-ip"
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := redirect.LoadIPRateLimitOverrides(path); err == nil {
		t.Fatal("expected invalid range to fail")
	}
}

func TestShippedGitHubActionsRateLimitConfig(t *testing.T) {
	overrides, err := redirect.LoadIPRateLimitOverrides(filepath.Join("..", "..", "github-actions-rate-limit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides = %d, want 1", len(overrides))
	}

	override := overrides[0]
	if override.RequestsPerMinute != 480 {
		t.Fatalf("requests per minute = %d, want 480", override.RequestsPerMinute)
	}
	if override.Burst != 960 {
		t.Fatalf("burst = %d, want 960", override.Burst)
	}
	if len(override.IPPrefixes) == 0 {
		t.Fatal("expected at least one GitHub Actions IP prefix")
	}
}
