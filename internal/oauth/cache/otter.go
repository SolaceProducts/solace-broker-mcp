package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/maypok86/otter/v2"
)

// otterTokenCache is the Otter v2-backed implementation of TokenCache.
// It is unexported — callers must use NewTokenCache.
type otterTokenCache struct {
	cache     *otter.Cache[string, CachedCredential]
	clockSkew time.Duration
	maxTTL    time.Duration
}

// newOtterTokenCache builds an Otter cache with cfg.MaxSize capacity and wraps
// it in an otterTokenCache. Returns an error if Otter's builder fails.
func newOtterTokenCache(cfg CacheConfig) (*otterTokenCache, error) {
	clockSkew := cfg.ClockSkew
	maxTTL := cfg.MaxTTL
	c, err := otter.New(&otter.Options[string, CachedCredential]{
		MaximumSize: cfg.MaxSize,
		ExpiryCalculator: otter.ExpiryWritingFunc[string, CachedCredential](func(entry otter.Entry[string, CachedCredential]) time.Duration {
			raw := time.Until(entry.Value.ExpiresAt) - clockSkew
			ttl := min(raw, maxTTL)
			if ttl <= 0 {
				return 0
			}
			return ttl
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("cache: build otter: %w", err)
	}
	return &otterTokenCache{
		cache:     c,
		clockSkew: clockSkew,
		maxTTL:    maxTTL,
	}, nil
}

func (o *otterTokenCache) Get(_ context.Context, key string) (GetResult, error) {
	entry, ok := o.cache.GetIfPresent(key)
	if !ok {
		return GetResult{Status: GetMiss}, nil
	}
	// Defend against future backend precision changes: verify freshness against
	// wall-clock even though v2 has nanosecond expiration precision.
	if !time.Now().Before(entry.ExpiresAt) {
		return GetResult{Status: GetMiss}, nil
	}
	return GetResult{Entry: entry, Status: GetHit}, nil
}

func (o *otterTokenCache) Put(_ context.Context, key string, entry CachedCredential) (PutResult, error) {
	rawTTL := time.Until(entry.ExpiresAt)
	safeTTL := rawTTL - o.clockSkew
	ttl := min(safeTTL, o.maxTTL)
	if ttl <= 0 {
		return PutResult{Status: PutDroppedTTL}, nil
	}
	o.cache.Set(key, entry)
	return PutResult{Status: PutStored}, nil
}

func (o *otterTokenCache) Delete(_ context.Context, key string) (DeleteResult, error) {
	o.cache.Invalidate(key)
	return DeleteResult{}, nil
}

func (o *otterTokenCache) Close() error {
	o.cache.StopAllGoroutines()
	return nil
}
