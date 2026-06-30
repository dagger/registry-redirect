package redirect

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
)

type IPRateLimitOverride struct {
	RequestsPerMinute int
	Burst             int
	IPPrefixes        []netip.Prefix
}

type IPRateLimitConfig struct {
	RateLimit IPRateLimitConfigRateLimit `json:"rate_limit"`
	IPRanges  []string                   `json:"ip_ranges"`
}

type IPRateLimitConfigRateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute"`
	Burst             int `json:"burst"`
}

func LoadIPRateLimitOverrides(path string) ([]IPRateLimitOverride, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()

	var config IPRateLimitConfig
	if err := dec.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode IP rate limit config %q: %w", path, err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("decode IP rate limit config %q: %w", path, err)
	}

	override, err := config.Override()
	if err != nil {
		return nil, fmt.Errorf("validate IP rate limit config %q: %w", path, err)
	}
	return []IPRateLimitOverride{override}, nil
}

func (c IPRateLimitConfig) Override() (IPRateLimitOverride, error) {
	if c.RateLimit.RequestsPerMinute <= 0 {
		return IPRateLimitOverride{}, fmt.Errorf("rate_limit.requests_per_minute must be greater than 0")
	}
	if c.RateLimit.Burst <= 0 {
		return IPRateLimitOverride{}, fmt.Errorf("rate_limit.burst must be greater than 0")
	}
	if len(c.IPRanges) == 0 {
		return IPRateLimitOverride{}, fmt.Errorf("ip_ranges must contain at least one IP address or CIDR range")
	}

	prefixes := make([]netip.Prefix, 0, len(c.IPRanges))
	for i, ipRange := range c.IPRanges {
		prefix, err := parseIPPrefix(ipRange)
		if err != nil {
			return IPRateLimitOverride{}, fmt.Errorf("ip_ranges[%d]: %w", i, err)
		}
		prefixes = append(prefixes, prefix)
	}

	return IPRateLimitOverride{
		RequestsPerMinute: c.RateLimit.RequestsPerMinute,
		Burst:             c.RateLimit.Burst,
		IPPrefixes:        prefixes,
	}, nil
}

func parseIPPrefix(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Prefix{}, fmt.Errorf("empty IP address or CIDR range")
	}

	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func ensureEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
