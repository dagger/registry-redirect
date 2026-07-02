package redirect

import (
	"net/http"
	"time"
)

const (
	defaultRateLimitRequestsPerMinute = 240
	defaultRateLimitBurst             = 480
	defaultRateLimitIdleTTL           = 10 * time.Minute
	defaultRateLimitMaxIPs            = 10000
	defaultManifestCacheMaxBytes      = 256 * 1024 * 1024
	defaultBlobCacheMaxBytes          = 512 * 1024 * 1024
	defaultBackendRequestTimeout      = 5 * time.Second
)

type Options struct {
	Transport     http.RoundTripper
	Client        *http.Client
	RateLimit     RateLimitOptions
	ManifestCache ManifestCacheOptions
	BlobCache     BlobCacheOptions
}

type RateLimitOptions struct {
	Disabled          bool
	RequestsPerMinute int
	Burst             int
	IdleTTL           time.Duration
	MaxIPs            int
	IPOverrides       []IPRateLimitOverride
	LimitedIPs        *RateLimitedIPTracker
}

type ManifestCacheOptions struct {
	Disabled bool
	MaxBytes int64
}

type BlobCacheOptions struct {
	Disabled bool
	MaxBytes int64
}

func DefaultOptions() Options {
	transport := http.DefaultTransport
	return Options{
		Transport: transport,
		Client: &http.Client{
			Transport: transport,
			Timeout:   defaultBackendRequestTimeout,
		},
		RateLimit: RateLimitOptions{
			RequestsPerMinute: defaultRateLimitRequestsPerMinute,
			Burst:             defaultRateLimitBurst,
			IdleTTL:           defaultRateLimitIdleTTL,
			MaxIPs:            defaultRateLimitMaxIPs,
		},
		ManifestCache: ManifestCacheOptions{
			MaxBytes: defaultManifestCacheMaxBytes,
		},
		BlobCache: BlobCacheOptions{
			MaxBytes: defaultBlobCacheMaxBytes,
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
	} else if o.Client.Timeout == 0 {
		client := *o.Client
		client.Timeout = defaultBackendRequestTimeout
		o.Client = &client
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
	if o.BlobCache.MaxBytes <= 0 {
		o.BlobCache.MaxBytes = defaults.BlobCache.MaxBytes
	}
	return o
}
