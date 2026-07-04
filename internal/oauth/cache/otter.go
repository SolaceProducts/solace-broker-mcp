package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/maypok86/otter"
)

// otterTokenCache is the Otter v2-backed implementation of TokenCache.
// It is unexported — callers must use NewTokenCache.
type otterTokenCache struct {
	cache     otter.CacheWithVariableTTL[string, CachedCredential]
	clockSkew time.Duration
	maxTTL    time.Duration
}

// newOtterTokenCache builds an Otter cache with cfg.MaxSize capacity and wraps
// it in an otterTokenCache. Returns an error if Otter's builder fails.
func newOtterTokenCache(cfg CacheConfig) (*otterTokenCache, error) {
	c, err := otter.MustBuilder[string, CachedCredential](cfg.MaxSize).
		WithVariableTTL().
		Build()
	if err != nil {
		return nil, fmt.Errorf("cache: build otter: %w", err)
	}
	return &otterTokenCache{
		cache:     c,
		clockSkew: cfg.ClockSkew,
		maxTTL:    cfg.MaxTTL,
	}, nil
}

// Get returns a token only if it is stored and still fresh. Belt-and-suspenders
// check against token.ExpiresAt guards against any Otter sweeper lag.
func (o *otterTokenCache) Get(_ context.Context, key string) (CachedCredential, bool, error) {
	entry, ok := o.cache.Get(key)
	if !ok {
		return CachedCredential{}, false, nil
	}
	// Belt-and-suspenders: Otter's TTL should have evicted this, but check anyway.
	if !time.Now().Before(entry.ExpiresAt) {
		return CachedCredential{}, false, nil
	}
	return entry, true, nil
}

// Put stores the token with a TTL derived from ExpiresAt minus ClockSkew,
// capped at MaxTTL. Does not store if the computed TTL is zero or negative.
func (o *otterTokenCache) Put(_ context.Context, key string, entry CachedCredential) error {
	rawTTL := time.Until(entry.ExpiresAt)
	safeTTL := rawTTL - o.clockSkew
	ttl := min(safeTTL, o.maxTTL)
	if ttl <= 0 {
		return nil
	}
	o.cache.Set(key, entry, ttl)
	return nil
}

// Delete removes the key from the cache immediately. Idempotent.
func (o *otterTokenCache) Delete(_ context.Context, key string) error {
	o.cache.Delete(key)
	return nil
}
