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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestCache constructs a TokenCache from cfg and registers t.Cleanup to
// call Close() on the interface, preventing goroutine leaks (Otter's sweeper)
// and unpinning the cache from the GC between tests.
//
// External packages should not duplicate this helper — use
// internal/oauth/cache/cachetest.Default(t) instead. This local copy exists
// because the cache package's own tests cannot import cachetest without
// creating an import cycle.
func newTestCache(t *testing.T, cfg CacheConfig) TokenCache {
	t.Helper()
	c, err := NewTokenCache(cfg)
	if err != nil {
		t.Fatalf("NewTokenCache: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("cache.Close: %v", err)
		}
	})
	return c
}

var defaultCfg = CacheConfig{
	MaxSize: 100,
	MaxTTL:  time.Hour,
}

// T1: Get returns only fresh tokens.
func TestGet_ReturnsFreshTokensOnly(t *testing.T) {
	t.Parallel()

	t.Run("fresh token is returned", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		tok := CachedCredential{Value: "tok", ExpiresAt: time.Now().Add(5 * time.Minute)}

		if _, err := c.Put(ctx, "k", tok); err != nil {
			t.Fatalf("Put: %v", err)
		}
		res, err := c.Get(ctx, "k")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if res.Status != GetHit {
			t.Fatalf("expected GetHit, got %v", res.Status)
		}
		if res.Entry.Value != tok.Value {
			t.Errorf("Value: got %q, want %q", res.Entry.Value, tok.Value)
		}
		if !res.Entry.ExpiresAt.Equal(tok.ExpiresAt) {
			t.Errorf("ExpiresAt: got %v, want %v", res.Entry.ExpiresAt, tok.ExpiresAt)
		}
	})

	t.Run("expired token is not returned (Put short-circuits on ttl<=0)", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		tok := CachedCredential{Value: "old", ExpiresAt: time.Now().Add(-5 * time.Minute)}

		pr, err := c.Put(ctx, "k2", tok)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if pr.Status != PutDroppedTTL {
			t.Errorf("expected PutDroppedTTL, got %v", pr.Status)
		}
		res, err := c.Get(ctx, "k2")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if res.Status != GetMiss {
			t.Errorf("expected GetMiss, got %v", res.Status)
		}
	})
}

// T2: Delete one key does not affect another key (key isolation).
func TestDelete_KeyIsolation(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok1 := CachedCredential{Value: "v1", ExpiresAt: time.Now().Add(10 * time.Minute)}
	tok2 := CachedCredential{Value: "v2", ExpiresAt: time.Now().Add(10 * time.Minute)}

	if _, err := c.Put(ctx, "k1", tok1); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if _, err := c.Put(ctx, "k2", tok2); err != nil {
		t.Fatalf("Put k2: %v", err)
	}

	if _, err := c.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete k1: %v", err)
	}

	res, err := c.Get(ctx, "k2")
	if err != nil {
		t.Fatalf("Get k2: %v", err)
	}
	if res.Status != GetHit {
		t.Error("expected k2 still present after deleting k1")
	}
	if res.Entry.Value != "v2" {
		t.Errorf("k2 Value: got %q, want %q", res.Entry.Value, "v2")
	}
}

// T3: Delete removes an entry that was previously found.
func TestDelete_RemovesEntry(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if _, err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := c.Get(ctx, "k")
	if err != nil || res.Status != GetHit {
		t.Fatalf("pre-delete Get: status=%v err=%v", res.Status, err)
	}

	if _, err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	res, err = c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("post-delete Get: %v", err)
	}
	if res.Status != GetMiss {
		t.Error("expected GetMiss after Delete")
	}
}

// T4: Delete is idempotent — both on a never-stored key and on double-delete.
func TestDelete_Idempotent(t *testing.T) {
	t.Parallel()

	t.Run("never-stored key", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		if _, err := c.Delete(ctx, "never-stored"); err != nil {
			t.Errorf("Delete(never-stored): expected nil, got %v", err)
		}
	})

	t.Run("double delete", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(10 * time.Minute)}
		if _, err := c.Put(ctx, "k", tok); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := c.Delete(ctx, "k"); err != nil {
			t.Errorf("first Delete: %v", err)
		}
		if _, err := c.Delete(ctx, "k"); err != nil {
			t.Errorf("second Delete: %v", err)
		}
	})
}

// T5: Concurrent access under -race flag.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 1000, MaxTTL: time.Hour})
	ctx := context.Background()

	const n = 50
	expiresAt := time.Now().Add(10 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(n * 2)

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			tok := CachedCredential{Value: fmt.Sprintf("val-%d", i), ExpiresAt: expiresAt}
			if _, err := c.Put(ctx, key, tok); err != nil {
				t.Errorf("Put(%s): %v", key, err)
			}
		}()
	}

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			res, err := c.Get(ctx, key)
			if err != nil {
				t.Errorf("Get(%s): %v", key, err)
			}
			if res.Status == GetHit && res.Entry.ExpiresAt.IsZero() {
				t.Errorf("Get(%s): GetHit but ExpiresAt is zero", key)
			}
		}()
	}

	wg.Wait()
}

// T6: TTL normal case — a token with a comfortably future ExpiresAt is
// returned immediately.
func TestTTL_NormalCase(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, MaxTTL: 24 * time.Hour})
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(2 * time.Minute)}
	if _, err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetHit {
		t.Error("expected GetHit for token well within TTL")
	}
}

// T7: TTL MaxTTL cap — token with long ExpiresAt is capped to MaxTTL but still positive.
func TestTTL_MaxTTLCap(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, MaxTTL: time.Minute})
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetHit {
		t.Error("expected GetHit after MaxTTL cap with positive remaining TTL")
	}
}

// T8 (SOL-154165): the cache deducts no safety margin of its own. ExpiresAt
// arrives with the producer's clock skew already applied, so the admission
// boundary is ExpiresAt itself: any remaining lifetime, however short, is
// stored, and only an entry at or past ExpiresAt is refused.
//
// The short-but-positive case is the regression guard. The cache used to
// subtract a second 30s skew here, which refused entries like this one — the
// defect that made the cache stop retaining anything for a short-lived-token
// IdP.
//
// Admitting an arbitrarily short lifetime is only safe because Get enforces
// ExpiresAt itself; TestGet_NeverServesPastExpiresAt covers what that guard
// is load-bearing for.
func TestTTL_NoMarginBeyondExpiresAt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		remaining time.Duration
		want      PutStatus
	}{
		{name: "well within lifetime", remaining: 2 * time.Minute, want: PutStored},
		// Shorter than the 30s skew the producer already applied, and
		// shorter still than the 60s the double deduction demanded.
		{name: "seconds of lifetime left", remaining: 20 * time.Second, want: PutStored},
		{name: "two seconds of lifetime left", remaining: 2 * time.Second, want: PutStored},
		{name: "already at expiry", remaining: 0, want: PutDroppedTTL},
		{name: "already past expiry", remaining: -time.Second, want: PutDroppedTTL},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCache(t, CacheConfig{MaxSize: 100, MaxTTL: 24 * time.Hour})
			ctx := context.Background()

			tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(tc.remaining)}
			pr, err := c.Put(ctx, "k", tok)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if pr.Status != tc.want {
				t.Errorf("Put status: got %v, want %v", pr.Status, tc.want)
			}

			// Put's status and what Get can see must agree, or Put reported
			// Stored on an entry the backend never retained.
			wantGet := GetHit
			if tc.want == PutDroppedTTL {
				wantGet = GetMiss
			}
			res, err := c.Get(ctx, "k")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if res.Status != wantGet {
				t.Errorf("Get status: got %v, want %v", res.Status, wantGet)
			}
		})
	}
}

// TestGet_NeverServesPastExpiresAt pins the one contract a credential cache
// cannot get wrong: an entry at or past its ExpiresAt is never handed back,
// whatever the backend does with it.
//
// This is not a theoretical guard. Otter installs a computed expiry only when
// it is strictly positive and a fresh node starts out marked "never expires",
// so a TTL that decays to zero between Put's guard and Otter's write used to
// leave the entry IMMORTAL rather than evicted — the exact inverse of the
// intended behavior. Measured before the fix: every one of ~13000 entries
// admitted with a sliver of life left was still served indefinitely afterwards.
//
// Reaching that window means Put's admission check and Otter's expiry
// calculator must read the clock either side of the entry's expiry. Rather than
// race a sub-microsecond lifetime and hope, the clock seam makes it exact: the
// first read (Put's guard) sees the entry alive, every later read (the
// calculator, then Get) sees it expired.
//
// Both halves of the fix are asserted, because either alone satisfies the
// first: Get must not serve the entry, AND the backend must not still hold it.
// The retention check is what pins the 1ns floor — without it the entry is
// merely masked by Get's check while occupying capacity forever.
func TestGet_NeverServesPastExpiresAt(t *testing.T) {
	t.Parallel()

	o, err := newOtterTokenCache(CacheConfig{MaxSize: 100, MaxTTL: time.Hour})
	if err != nil {
		t.Fatalf("newOtterTokenCache: %v", err)
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})
	ctx := context.Background()

	base := time.Now()
	expiresAt := base.Add(time.Second)
	var reads atomic.Int64
	o.now = func() time.Time {
		if reads.Add(1) == 1 {
			return base // Put's guard: one second of life left.
		}
		return base.Add(time.Hour) // decayed past expiry by the time of the write.
	}

	pr, err := o.Put(ctx, "k", CachedCredential{Value: "v", ExpiresAt: expiresAt})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Put admitted it on the strength of the first clock read. That is the
	// premise of the test, not the thing under test — if it ever stops holding,
	// the window below is no longer being exercised.
	if pr.Status != PutStored {
		t.Fatalf("Put status: got %v, want PutStored; the admission window this test guards was not entered", pr.Status)
	}
	if n := reads.Load(); n < 2 {
		t.Fatalf("clock was read %d times, want at least 2 (Put's guard and the expiry calculator); the calculator no longer consults it", n)
	}

	res, err := o.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetMiss {
		t.Errorf("Get: got %v, want GetMiss — an entry past its ExpiresAt was handed back", res.Status)
	}
	if _, present := o.cache.GetIfPresent("k"); present {
		t.Error("the backend still holds the entry after its expiry: a zero TTL was installed as 'never expires' instead of being floored to a short one")
	}
}

// TestDeriveTTL_DeductsNoSkew pins the arithmetic behind T8 to the nanosecond:
// the installed TTL is the raw remaining lifetime, capped at MaxTTL, with
// nothing subtracted. deriveTTL takes its clock as a parameter precisely so
// this can be exact — there is no tolerance for a re-introduced deduction of
// any size to hide inside.
func TestDeriveTTL_DeductsNoSkew(t *testing.T) {
	t.Parallel()

	const maxTTL = time.Hour
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{name: "one nanosecond", remaining: time.Nanosecond, want: time.Nanosecond},
		{name: "one second", remaining: time.Second, want: time.Second},
		// 30s and 60s are the values the double deduction destroyed: with a
		// 30s skew applied here they derived 0s and 30s instead.
		{name: "thirty seconds", remaining: 30 * time.Second, want: 30 * time.Second},
		{name: "sixty seconds", remaining: 60 * time.Second, want: 60 * time.Second},
		{name: "ten minutes", remaining: 10 * time.Minute, want: 10 * time.Minute},
		{name: "capped at maxTTL", remaining: 24 * time.Hour, want: maxTTL},
		{name: "at expiry", remaining: 0, want: 0},
		{name: "past expiry", remaining: -time.Minute, want: 0},
		{name: "a century past expiry", remaining: -100 * 365 * 24 * time.Hour, want: 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveTTL(now, now.Add(tc.remaining), maxTTL); got != tc.want {
				t.Errorf("deriveTTL(now, now+%v, %v) = %v, want exactly %v",
					tc.remaining, maxTTL, got, tc.want)
			}
		})
	}

	// A zero-value ExpiresAt is the one input that SATURATES: Time.Sub clamps
	// to minDuration rather than wrapping, and only year-1 does that (a mere
	// century back is -876000h, nowhere near the -2562047h47m limit, so the
	// row above does not reach this case).
	//
	// It has to be pinned separately because it needs an absolute expiresAt,
	// not an offset. It is the input the pre-fix signature got catastrophically
	// wrong: subtracting the skew from minDuration UNDERFLOWED to a large
	// positive duration, which min() then clamped to maxTTL — so an
	// epoch-zero credential was retained for the full 24h in production
	// instead of being dropped. With nothing subtracted there is nothing to
	// underflow, and it must stay dropped.
	t.Run("zero-value expiry saturates and is dropped", func(t *testing.T) {
		t.Parallel()
		var zeroTime time.Time
		if got := deriveTTL(now, zeroTime, maxTTL); got != 0 {
			t.Errorf("deriveTTL(now, time.Time{}, %v) = %v, want exactly 0", maxTTL, got)
		}
		// Pins the premise: if Sub ever stopped saturating here, the case above
		// would silently stop covering the underflow it exists for.
		if got := zeroTime.Sub(now); got != time.Duration(math.MinInt64) {
			t.Errorf("time.Time{}.Sub(now) = %v, want minDuration; this case no longer saturates", got)
		}
	})
}

// TestGet_RefusesEntryTheBackendStillHolds pins Get's freshness check by the
// only means that isolates it: an entry Otter is still holding, refused anyway
// because it is past its own ExpiresAt. Both subtests assert the entry is still
// present in the backend afterwards, so the miss provably comes from the check
// and not from eviction.
//
// Read the clock semantics carefully before touching this. Time.Before compares
// the MONOTONIC readings alone when both operands carry one, ignoring the wall
// clock entirely. Advancing a fake clock with start.Add(d) moves the monotonic
// reading too, so it models d of elapsed time — NOT a wall-clock step. A test
// written that way passes against a plain time.Sleep and proves nothing about
// clock jumps. The two cases below therefore keep the clock families straight:
// the first is the elapsed-time path that runs in production, the second strips
// the monotonic readings (time.Time.Round(0)) to reach the wall-clock
// comparison, which is the path a future producer with a JWT `exp` or a
// JSON-round-tripped expiry would take.
func TestGet_RefusesEntryTheBackendStillHolds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// expiresAt is what gets cached; nowAfter is what o.now returns for the
		// second lookup. Both are built from the same start instant.
		expiresAt func(start time.Time) time.Time
		nowAfter  func(start time.Time) time.Time
	}{
		{
			// Production shape: both operands carry monotonic readings, so the
			// comparison is monotonic and this is an elapsed-time scenario.
			name:      "monotonic operands, time elapsed past the expiry",
			expiresAt: func(start time.Time) time.Time { return start.Add(time.Hour) },
			nowAfter:  func(start time.Time) time.Time { return start.Add(2 * time.Hour) },
		},
		{
			// Round(0) strips the monotonic reading from both sides, so Before
			// falls back to comparing wall clocks. This is reason 3 in Get's
			// doc comment, and the only case here that exercises it.
			name: "wall-clock operands, clock stepped past the expiry",
			expiresAt: func(start time.Time) time.Time {
				return start.Add(time.Hour).Round(0)
			},
			nowAfter: func(start time.Time) time.Time {
				return start.Round(0).Add(2 * time.Hour)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o, err := newOtterTokenCache(CacheConfig{MaxSize: 100, MaxTTL: time.Hour})
			if err != nil {
				t.Fatalf("newOtterTokenCache: %v", err)
			}
			t.Cleanup(func() {
				if err := o.Close(); err != nil {
					t.Logf("Close: %v", err)
				}
			})
			ctx := context.Background()

			// An hour of real life, so Otter's own timer holds the entry
			// throughout and cannot be what produces the miss below.
			start := time.Now()
			pr, err := o.Put(ctx, "k", CachedCredential{Value: "v", ExpiresAt: tc.expiresAt(start)})
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if pr.Status != PutStored {
				t.Fatalf("Put status: got %v, want PutStored", pr.Status)
			}
			if res, err := o.Get(ctx, "k"); err != nil || res.Status != GetHit {
				t.Fatalf("Get before expiry: status=%v err=%v, want GetHit", res.Status, err)
			}

			o.now = func() time.Time { return tc.nowAfter(start) }

			res, err := o.Get(ctx, "k")
			if err != nil {
				t.Fatalf("Get after expiry: %v", err)
			}
			if res.Status != GetMiss {
				t.Errorf("Get after expiry: got %v, want GetMiss — an entry past its ExpiresAt was handed back because the backend's own timer had not fired yet", res.Status)
			}
			// Confirms the miss came from the freshness check rather than from
			// eviction; without this the test would pass for the wrong reason.
			if _, present := o.cache.GetIfPresent("k"); !present {
				t.Error("the backend dropped the entry, so this test no longer isolates Get's freshness check")
			}
		})
	}
}

// T9: GetMiss is returned for a never-stored key. GetMiss is the single
// non-hit status the wrapper produces; the backend surfaces expired entries
// as absent internally, so no separate expired-hit status exists.
func TestGetResult_MissStatus(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	res, err := c.Get(ctx, "never-stored")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetMiss {
		t.Errorf("got %v, want GetMiss", res.Status)
	}
	if res.Entry != (CachedCredential{}) {
		t.Errorf("expected zero CachedCredential on miss, got %+v", res.Entry)
	}
}

// T11: Put overwrites an existing key with a new value.
func TestPut_OverwritesExistingKey(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()
	exp := time.Now().Add(10 * time.Minute)

	if _, err := c.Put(ctx, "k", CachedCredential{Value: "token1", ExpiresAt: exp}); err != nil {
		t.Fatalf("Put token1: %v", err)
	}
	if _, err := c.Put(ctx, "k", CachedCredential{Value: "token2", ExpiresAt: exp}); err != nil {
		t.Fatalf("Put token2: %v", err)
	}

	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetHit {
		t.Fatal("expected GetHit after overwrite")
	}
	if res.Entry.Value != "token2" {
		t.Errorf("Value: got %q, want %q", res.Entry.Value, "token2")
	}
}

// T12: MaxSize=0 returns an error at construction.
func TestNewTokenCache_ErrorOnMaxSizeZero(t *testing.T) {
	t.Parallel()
	_, err := NewTokenCache(CacheConfig{MaxSize: 0, MaxTTL: time.Hour})
	if err == nil {
		t.Fatal("expected error for MaxSize=0, got nil")
	}
}

// T12a: MaxTTL<=0 returns an error at construction. A zero or negative MaxTTL
// would clamp every safeTTL to <= 0 in Put, causing every entry to be silently
// dropped as PutDroppedTTL — a valid-looking cache that never caches anything.
// Fail loud at construction instead.
func TestNewTokenCache_ErrorOnMaxTTLZero(t *testing.T) {
	t.Parallel()
	_, err := NewTokenCache(CacheConfig{MaxSize: 100, MaxTTL: 0})
	if err == nil {
		t.Fatal("expected error for MaxTTL=0, got nil")
	}
}

func TestNewTokenCache_ErrorOnMaxTTLNegative(t *testing.T) {
	t.Parallel()
	_, err := NewTokenCache(CacheConfig{MaxSize: 100, MaxTTL: -time.Second})
	if err == nil {
		t.Fatal("expected error for negative MaxTTL, got nil")
	}
}

// T14: All Put/Get/Delete calls return nil error, including edge cases.
func TestErrors_AlwaysNil(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	if _, err := c.Put(ctx, "fresh", CachedCredential{Value: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Errorf("Put(fresh): %v", err)
	}

	if _, err := c.Put(ctx, "expired", CachedCredential{Value: "v", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Errorf("Put(expired): %v", err)
	}

	if _, err := c.Get(ctx, "fresh"); err != nil {
		t.Errorf("Get(fresh): %v", err)
	}

	if _, err := c.Get(ctx, "nonexistent"); err != nil {
		t.Errorf("Get(nonexistent): %v", err)
	}

	if _, err := c.Delete(ctx, "fresh"); err != nil {
		t.Errorf("Delete(fresh): %v", err)
	}

	if _, err := c.Delete(ctx, "nonexistent"); err != nil {
		t.Errorf("Delete(nonexistent): %v", err)
	}
}

// Test B: Stale token is not served after natural expiry (belt-and-suspenders).
func TestGet_StaleNotServedAfterExpiry(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok := CachedCredential{Value: "short", ExpiresAt: time.Now().Add(1 * time.Second)}
	pr, err := c.Put(ctx, "k", tok)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if pr.Status != PutStored {
		t.Fatalf("expected PutStored, got %v", pr.Status)
	}

	// Verify it's there.
	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	if res.Status != GetHit {
		t.Fatalf("expected GetHit before expiry, got %v", res.Status)
	}

	time.Sleep(2 * time.Second)

	res, err = c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if res.Status == GetHit {
		t.Error("stale token should not be served after expiry")
	}
}

// Test C: PutResult reports PutStored for a successfully stored entry.
func TestPutResult_ReportsStored(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(5 * time.Minute)}
	pr, err := c.Put(ctx, "k", tok)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if pr.Status != PutStored {
		t.Errorf("expected PutStored, got %v", pr.Status)
	}
}

// TestGetResult_LogValueDoesNotLeakToken pins the log-safety contract for
// GetResult. Without a LogValue method on the result type, slog.Any falls
// back to reflection and walks straight into Entry.Value — leaking the
// raw broker-bound credential to any log line that passes the result in
// verbatim. CachedCredential's own LogValue is bypassed because slog only
// consults it if the enclosing type does not itself implement LogValuer.
// This is exactly the landmine docs/internal/secure-logging-rules.md warns
// about.
func TestGetResult_LogValueDoesNotLeakToken(t *testing.T) {
	t.Parallel()

	const secret = "SUPER-SECRET-BEARER-TOKEN-VALUE"
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))

	gr := GetResult{
		Entry:  CachedCredential{Value: secret, ExpiresAt: time.Now().Add(time.Hour)},
		Status: GetHit,
	}
	l.LogAttrs(context.Background(), slog.LevelInfo, "test", slog.Any("cache_result", gr))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("GetResult leaks Entry.Value through slog.Any (regression of the LogValue guard). Emitted line: %s", out)
	}
	if !strings.Contains(out, `"status":"hit"`) {
		t.Errorf("GetResult log should surface status=hit; got: %s", out)
	}
}

// TestCachedCredential_RenderersRedactTheToken pins the credential-redaction
// contract on CachedCredential's three renderers. Each one exists solely to
// keep Value out of output, and each is reached by a different formatting path
// — %v and %s go through String, %#v goes through GoString, and slog resolves
// LogValue rather than either of them — so a leak plugged in one is
// still open in the others. None of the three was exercised by a test, which
// is precisely how such a leak returns unnoticed.
//
// See docs/internal/secure-logging-rules.md. If this fails, fix the renderer,
// not this test.
func TestCachedCredential_RenderersRedactTheToken(t *testing.T) {
	t.Parallel()

	const secret = "SUPER-SECRET-BEARER-TOKEN-VALUE"
	expiresAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := CachedCredential{Value: secret, ExpiresAt: expiresAt}

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("m", "cred", c)

	// %v and %s both resolve through Stringer, so %v stands for both.
	renderings := map[string]string{
		"String()":      c.String(),
		"GoString()":    c.GoString(),
		"%v":            fmt.Sprintf("%v", c),
		"%#v":           fmt.Sprintf("%#v", c),
		"slog LogValue": logged.String(),
	}

	for path, got := range renderings {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaks the token value: %s", path, got)
		}
		// A renderer that emits nothing useful is not redaction done well —
		// the expiry is the one field that makes these lines diagnosable.
		if !strings.Contains(got, "2026") {
			t.Errorf("%s dropped the expiry, leaving nothing diagnosable: %s", path, got)
		}
	}
}

// TestPutResult_LogValueDoesNotReflectTheStruct is the Put-side counterpart to
// TestGetResult_LogValueDoesNotLeakToken. PutResult carries no credential
// today, so the guard is the shape rather than a secret: slog must render the
// declared status group and not reflect over the struct, which is what would
// silently surface a future field.
func TestPutResult_LogValueDoesNotReflectTheStruct(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", slog.Any("cache_result", PutResult{Status: PutDroppedTTL}))

	if got := buf.String(); !strings.Contains(got, `"cache_result":{"status":"dropped_ttl"}`) {
		t.Errorf("emitted %s, want a status group rendering dropped_ttl", strings.TrimSpace(got))
	}
}

// TestStatusString_UnknownValue covers the default arms of both Stringers. A
// status added later without a String case must still render as something a
// reader can trace back to an enum, never as a bare integer that reads like an
// exit code.
func TestStatusString_UnknownValue(t *testing.T) {
	t.Parallel()

	if got, want := GetStatus(99).String(), "GetStatus(99)"; got != want {
		t.Errorf("GetStatus(99).String() = %q, want %q", got, want)
	}
	if got, want := PutStatus(99).String(), "PutStatus(99)"; got != want {
		t.Errorf("PutStatus(99).String() = %q, want %q", got, want)
	}
}

// TestNewTokenCache_ValidationFirstFailureWins pins the caller-observable
// contract that when multiple CacheConfig fields are invalid, the constructor
// reports only the FIRST failing field, in the fixed order MaxSize → MaxTTL.
// Callers rely on this to fix misconfiguration one field at a time without the
// error message swimming around as they patch each field.
//
// The assertion is deliberately loose on wording — it only checks that the
// expected field name appears in the error. The exact phrasing is not part of
// the contract.
func TestNewTokenCache_ValidationFirstFailureWins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		cfg       CacheConfig
		wantField string
	}{
		{
			name:      "both invalid, MaxSize reported first",
			cfg:       CacheConfig{MaxSize: 0, MaxTTL: 0},
			wantField: "MaxSize",
		},
		{
			name:      "only MaxTTL invalid, MaxTTL reported",
			cfg:       CacheConfig{MaxSize: 100, MaxTTL: -time.Second},
			wantField: "MaxTTL",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewTokenCache(tc.cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if c != nil {
				t.Errorf("expected nil cache on error, got %T", c)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("expected error mentioning %q, got: %v", tc.wantField, err)
			}
		})
	}
}

// TestPut_MaxTTLCapPreservesCallerExpiresAt pins the two-clock behaviour
// callers depend on: when the caller's ExpiresAt is farther out than MaxTTL
// allows, the cache internally retains the entry for the shorter MaxTTL, but
// Get returns the caller's ORIGINAL ExpiresAt unchanged. The cache's internal
// eviction clock and the value the caller sees are decoupled — a subsequent
// backend must not silently "correct" the returned expiry to the shortened
// retention window.
func TestPut_MaxTTLCapPreservesCallerExpiresAt(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, MaxTTL: time.Hour})
	ctx := context.Background()

	callerExpiry := time.Now().Add(48 * time.Hour)
	tok := CachedCredential{Value: "v", ExpiresAt: callerExpiry}

	if _, err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetHit {
		t.Fatalf("expected GetHit, got %v", res.Status)
	}
	if !res.Entry.ExpiresAt.Equal(callerExpiry) {
		t.Errorf("ExpiresAt: got %v, want %v (caller's original value)", res.Entry.ExpiresAt, callerExpiry)
	}
}

// TestClose_ReturnsNil pins the first-call contract for Close: it returns nil.
// The wrapper does not promise anything about a second call; that is out of
// scope here on purpose.
func TestClose_ReturnsNil(t *testing.T) {
	t.Parallel()
	// Deliberately not using newTestCache — the test IS the Close.
	c, err := NewTokenCache(defaultCfg)
	if err != nil {
		t.Fatalf("NewTokenCache: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: expected nil, got %v", err)
	}
}

// TestConcurrentAccess_IncludesDelete pins that Delete is safe for concurrent
// use alongside Put and Get. The whole point is racing operations on
// overlapping keys: any legal outcome is fine, so this test asserts nothing
// about values. The -race detector is what enforces the contract; the
// assertion below is only that the test itself completes cleanly.
func TestConcurrentAccess_IncludesDelete(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 1000, MaxTTL: time.Hour})
	ctx := context.Background()

	const n = 50
	expiresAt := time.Now().Add(10 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(n * 3)

	// Overlap keys across Put/Get/Delete by using the same key-<i> for the
	// same i, so each iteration exercises all three operations on one key.
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			tok := CachedCredential{Value: fmt.Sprintf("val-%d", i), ExpiresAt: expiresAt}
			if _, err := c.Put(ctx, key, tok); err != nil {
				t.Errorf("Put(%s): %v", key, err)
			}
		}()
	}

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			if _, err := c.Get(ctx, key); err != nil {
				t.Errorf("Get(%s): %v", key, err)
			}
		}()
	}

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			if _, err := c.Delete(ctx, key); err != nil {
				t.Errorf("Delete(%s): %v", key, err)
			}
		}()
	}

	wg.Wait()
	t.Logf("ran %d Put, %d Get, %d Delete goroutines on overlapping keys", n, n, n)
}

// TestGetStatus_LogValue asserts the emitted log output, not the method return:
// slog resolves LogValuer and never String, so a status with only a String
// method serializes as its iota. Asserting through a real handler is what makes
// that visible — a unit check of String() passed the whole time these logs were
// unreadable.
func TestGetStatus_LogValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status GetStatus
		want   string
	}{
		{"hit", GetHit, `"result":"hit"`},
		{"miss", GetMiss, `"result":"miss"`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "result", tc.status)

			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("emitted %s, want it to contain %s", strings.TrimSpace(got), tc.want)
			}
			if bad := fmt.Sprintf(`"result":%d`, int(tc.status)); strings.Contains(buf.String(), bad) {
				t.Errorf("emitted the raw enum %s; LogValue is not being honored", bad)
			}
		})
	}
}

// TestPutStatus_LogValue is symmetric with TestGetStatus_LogValue. PutStored
// is 0 and GetHit is also 0, so a bare integer carries no signal about which
// enum — or which polarity — a reader is looking at.
func TestPutStatus_LogValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status PutStatus
		want   string
	}{
		{"stored", PutStored, `"result":"stored"`},
		{"dropped_ttl", PutDroppedTTL, `"result":"dropped_ttl"`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "result", tc.status)

			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("emitted %s, want it to contain %s", strings.TrimSpace(got), tc.want)
			}
			if bad := fmt.Sprintf(`"result":%d`, int(tc.status)); strings.Contains(buf.String(), bad) {
				t.Errorf("emitted the raw enum %s; LogValue is not being honored", bad)
			}
		})
	}
}
