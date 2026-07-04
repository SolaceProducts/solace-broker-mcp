package cache

import "fmt"

// NewTokenCache constructs a TokenCache backed by Otter v2 using the provided
// configuration. This is the only public constructor — no other package in the
// project instantiates the cache directly.
func NewTokenCache(cfg CacheConfig) (TokenCache, error) {
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("cache: MaxSize must be > 0, got %d", cfg.MaxSize)
	}
	return newOtterTokenCache(cfg)
}
