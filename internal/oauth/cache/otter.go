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
	cache  *otter.Cache[string, CachedCredential]
	maxTTL time.Duration
	// now is the wall clock every expiry decision in this type reads — Put's
	// admission check, the ExpiryCalculator, and Get — so all three agree by
	// construction, and tests can simulate a clock jump. Always time.Now in
	// production, assigned once at construction and never written again.
	//
	// Safe to read unsynchronized: ExpiryWritingFunc invokes the calculator
	// only from ExpireAfterCreate/ExpireAfterUpdate, both of which Otter runs
	// on the goroutine calling Set, so no maintenance goroutine ever reads
	// this. A test that reassigns it must still do so outside concurrent use.
	now func() time.Time
}

// newOtterTokenCache builds an Otter cache with cfg.MaxSize capacity and wraps
// it in an otterTokenCache. Returns an error if Otter's builder fails.
func newOtterTokenCache(cfg CacheConfig) (*otterTokenCache, error) {
	o := &otterTokenCache{
		maxTTL: cfg.MaxTTL,
		now:    time.Now,
	}
	c, err := otter.New(&otter.Options[string, CachedCredential]{
		MaximumSize: cfg.MaxSize,
		ExpiryCalculator: otter.ExpiryWritingFunc[string, CachedCredential](func(entry otter.Entry[string, CachedCredential]) time.Duration {
			// Never hand Otter a zero. Its calcExpiresAtAfterWrite installs the
			// computed expiry only `if expiresAfter > 0`, and a fresh node
			// starts out marked "never expires" — so returning 0 here does not
			// evict the entry, it makes the entry IMMORTAL. That is reachable
			// whenever the remaining lifetime decays past zero between Put's
			// guard and Otter's write, and it would leave an expired credential
			// served forever. Flooring at 1ns expires it at the next read.
			if ttl := deriveTTL(o.now(), entry.Value.ExpiresAt, o.maxTTL); ttl > 0 {
				return ttl
			}
			return time.Nanosecond
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("cache: build otter: %w", err)
	}
	o.cache = c
	return o, nil
}

// deriveTTL is the single source of truth for how a caller-supplied ExpiresAt
// becomes an installed backend TTL. Used by Put for its PutDroppedTTL
// short-circuit and by the ExpiryCalculator for the installed lifetime; both
// must agree, or Put reports an outcome the backend does not honor. Returns 0
// when the entry is already at or past its expiry.
//
// The two callers read the clock a moment apart, so they can disagree even
// with identical arithmetic: Put's guard can see a positive lifetime that has
// decayed to zero by the time the calculator runs. Neither caller may treat a
// zero as "the backend will drop it" — see the calculator above for what Otter
// actually does with a zero, and Get for the freshness check that makes the
// disagreement unobservable to callers.
//
// INVARIANT (SOL-154165): ExpiresAt arrives already carrying its clock-skew
// safety margin, so this function deducts NO further margin — the remaining
// lifetime is exactly expiresAt.Sub(now), capped at maxTTL. See
// CachedCredential.ExpiresAt for who owns the margin and why. Subtracting a
// skew here again is the bug this invariant exists to prevent: it cost every
// token 30s of cache lifetime and, once an IdP's token lifetime dropped to 60s
// or below, made the cache store nothing at all.
func deriveTTL(now, expiresAt time.Time, maxTTL time.Duration) time.Duration {
	ttl := min(expiresAt.Sub(now), maxTTL)
	if ttl <= 0 {
		return 0
	}
	return ttl
}

// Get returns a hit only for an entry still within its own ExpiresAt. Three
// reasons, in descending order of what they buy today:
//
//  1. Otter samples its own clock a moment AFTER Put reads o.now(), so the
//     expiry it installs lands marginally later than ExpiresAt. This closes
//     that sliver, in which an entry already past its use-by is still served.
//  2. It makes GetHit's "found and fresh" contract true whatever the backend
//     retains, instead of true only while our reading of a third-party
//     library's expiry semantics holds. The immortal-entry defect handled in
//     the calculator above is what that caveat costs when it slips: a guard
//     here fails safe, an assumption there does not.
//  3. It becomes real wall-clock protection for any future producer whose
//     ExpiresAt carries no monotonic reading — a JWT `exp` built with
//     time.Unix, or an L2 entry round-tripped through JSON. For those, Before
//     compares wall clocks, and a forward step is caught.
//
// Be clear about what this does NOT do today, because the obvious story is
// wrong. Today's only producer builds ExpiresAt as time.Now().Add(...), so
// both operands carry a monotonic reading, and Go then compares the monotonic
// readings ALONE and ignores the wall clock (see the time package's
// "Monotonic Clocks" section). A wall-clock step is therefore invisible here
// on the current path, exactly as it is to Otter. Reasons 1 and 2 are what
// earn this line now; reason 3 is latent.
func (o *otterTokenCache) Get(_ context.Context, key string) (GetResult, error) {
	entry, ok := o.cache.GetIfPresent(key)
	if !ok {
		return GetResult{Status: GetMiss}, nil
	}
	if !o.now().Before(entry.ExpiresAt) {
		return GetResult{Status: GetMiss}, nil
	}
	return GetResult{Entry: entry, Status: GetHit}, nil
}

func (o *otterTokenCache) Put(_ context.Context, key string, entry CachedCredential) (PutResult, error) {
	if deriveTTL(o.now(), entry.ExpiresAt, o.maxTTL) == 0 {
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
