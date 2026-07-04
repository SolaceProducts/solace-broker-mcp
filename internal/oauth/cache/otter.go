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

func (o *otterTokenCache) Get(_ context.Context, key string) (GetResult, error) {
	entry, ok := o.cache.Get(key)
	if !ok {
		return GetResult{Status: GetMissAbsent}, nil
	}
	if !time.Now().Before(entry.ExpiresAt) {
		return GetResult{Status: GetMissExpired}, nil
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
	if !o.cache.Set(key, entry, ttl) {
		return PutResult{Status: PutDroppedFull}, nil
	}
	return PutResult{Status: PutStored}, nil
}

func (o *otterTokenCache) Delete(_ context.Context, key string) (DeleteResult, error) {
	o.cache.Delete(key)
	return DeleteResult{}, nil
}
