package redirect

import (
	"net/http"
	"time"
)

const (
	defaultRateLimitRequestsPerMinute = 120
	defaultRateLimitBurst             = 240
	defaultRateLimitIdleTTL           = 10 * time.Minute
	defaultRateLimitMaxIPs            = 10000
	defaultManifestCacheMaxBytes      = 256 * 1024 * 1024
)

type Options struct {
	Transport     http.RoundTripper
	Client        *http.Client
	RateLimit     RateLimitOptions
	ManifestCache ManifestCacheOptions
}

type RateLimitOptions struct {
	Disabled          bool
	RequestsPerMinute int
	Burst             int
	IdleTTL           time.Duration
	MaxIPs            int
}

type ManifestCacheOptions struct {
	Disabled bool
	MaxBytes int64
}

func DefaultOptions() Options {
	return Options{
		Transport: http.DefaultTransport,
		Client:    http.DefaultClient,
		RateLimit: RateLimitOptions{
			RequestsPerMinute: defaultRateLimitRequestsPerMinute,
			Burst:             defaultRateLimitBurst,
			IdleTTL:           defaultRateLimitIdleTTL,
			MaxIPs:            defaultRateLimitMaxIPs,
		},
		ManifestCache: ManifestCacheOptions{
			MaxBytes: defaultManifestCacheMaxBytes,
		},
	}
}

func (o Options) withDefaults() Options {
	defaults := DefaultOptions()
	if o.Transport == nil {
		o.Transport = defaults.Transport
	}
	if o.Client == nil {
		o.Client = defaults.Client
	}
	if o.RateLimit.RequestsPerMinute <= 0 {
		o.RateLimit.RequestsPerMinute = defaults.RateLimit.RequestsPerMinute
	}
	if o.RateLimit.Burst <= 0 {
		o.RateLimit.Burst = defaults.RateLimit.Burst
	}
	if o.RateLimit.IdleTTL <= 0 {
		o.RateLimit.IdleTTL = defaults.RateLimit.IdleTTL
	}
	if o.RateLimit.MaxIPs <= 0 {
		o.RateLimit.MaxIPs = defaults.RateLimit.MaxIPs
	}
	if o.ManifestCache.MaxBytes <= 0 {
		o.ManifestCache.MaxBytes = defaults.ManifestCache.MaxBytes
	}
	return o
}
