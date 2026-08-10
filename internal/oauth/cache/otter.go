// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
			return deriveTTL(entry.Value.ExpiresAt, clockSkew, maxTTL)
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

// deriveTTL is the single source of truth for how a caller-supplied ExpiresAt
// becomes an installed backend TTL. Used by Put for its PutDroppedTTL
// short-circuit and by the ExpiryCalculator for the installed lifetime; both
// must agree or a Put would report Stored on an entry the backend won't retain.
// Returns 0 when the entry is already past its safe lifetime.
func deriveTTL(expiresAt time.Time, clockSkew, maxTTL time.Duration) time.Duration {
	ttl := min(time.Until(expiresAt)-clockSkew, maxTTL)
	if ttl <= 0 {
		return 0
	}
	return ttl
}

func (o *otterTokenCache) Get(_ context.Context, key string) (GetResult, error) {
	entry, ok := o.cache.GetIfPresent(key)
	if !ok {
		return GetResult{Status: GetMiss}, nil
	}
	return GetResult{Entry: entry, Status: GetHit}, nil
}

func (o *otterTokenCache) Put(_ context.Context, key string, entry CachedCredential) (PutResult, error) {
	if deriveTTL(entry.ExpiresAt, o.clockSkew, o.maxTTL) == 0 {
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
