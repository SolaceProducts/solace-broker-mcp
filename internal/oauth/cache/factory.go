package cache

import "fmt"

// NewTokenCache constructs a TokenCache backed by Otter v2 using the provided
// configuration. This is the only public constructor — no other package in the
// project instantiates the cache directly.
//
// All three CacheConfig fields are validated. MaxSize must be > 0; MaxTTL must
// be > 0; ClockSkew must be >= 0. A misconfigured MaxTTL of 0 (or negative)
// would silently make every Put drop as PutDroppedTTL, effectively disabling
// the cache — failing loud here surfaces the misconfiguration at startup
// rather than as a mysterious performance regression under load.
func NewTokenCache(cfg CacheConfig) (TokenCache, error) {
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("cache: MaxSize must be > 0, got %d", cfg.MaxSize)
	}
	if cfg.MaxTTL <= 0 {
		return nil, fmt.Errorf("cache: MaxTTL must be > 0, got %v", cfg.MaxTTL)
	}
	if cfg.ClockSkew < 0 {
		return nil, fmt.Errorf("cache: ClockSkew must be >= 0, got %v", cfg.ClockSkew)
	}
	return newOtterTokenCache(cfg)
}
