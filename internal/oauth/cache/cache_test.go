package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestCache constructs a TokenCache from cfg and registers t.Cleanup to call
// Close() on the underlying otter cache, preventing goroutine leaks between tests.
func newTestCache(t *testing.T, cfg CacheConfig) TokenCache {
	t.Helper()
	c, err := NewTokenCache(cfg)
	if err != nil {
		t.Fatalf("NewTokenCache: %v", err)
	}
	// otter.CacheWithVariableTTL embeds baseCache which exposes Close().
	// We reach it through the concrete type returned by newOtterTokenCache.
	t.Cleanup(func() {
		c.(*otterTokenCache).cache.Close()
	})
	return c
}

var defaultCfg = CacheConfig{
	MaxSize:   100,
	ClockSkew: 0,
	MaxTTL:    time.Hour,
}

// T1: Get returns only fresh tokens.
//
// Sub-case 2 note: a token with ExpiresAt in the past produces a non-positive TTL
// inside Put, so the entry is silently dropped before it ever reaches otter.
// The token is therefore absent from Get, not evicted — Put short-circuits on ttl<=0.
func TestGet_ReturnsFreshTokensOnly(t *testing.T) {
	t.Parallel()

	t.Run("fresh token is returned", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		tok := CachedCredential{Value: "tok", ExpiresAt: time.Now().Add(5 * time.Minute)}

		if err := c.Put(ctx, "k", tok); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, found, err := c.Get(ctx, "k")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !found {
			t.Fatal("expected found=true for fresh token")
		}
		if got.Value != tok.Value {
			t.Errorf("Value: got %q, want %q", got.Value, tok.Value)
		}
		if !got.ExpiresAt.Equal(tok.ExpiresAt) {
			t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, tok.ExpiresAt)
		}
	})

	t.Run("expired token is not returned (Put short-circuits on ttl<=0)", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		// ExpiresAt in the past → Put computes ttl<=0 and drops the entry silently.
		tok := CachedCredential{Value: "old", ExpiresAt: time.Now().Add(-5 * time.Minute)}

		if err := c.Put(ctx, "k2", tok); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, found, err := c.Get(ctx, "k2")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if found {
			t.Errorf("expected found=false for expired token, got found=true with value %q", got.Value)
		}
		if got != (CachedCredential{}) {
			t.Errorf("expected zero Token on miss, got %+v", got)
		}
	})
}

// T2: Put and Get round-trip across multiple keys.
func TestPutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok1 := CachedCredential{Value: "access-token-1", ExpiresAt: time.Now().Add(10 * time.Minute)}
	tok2 := CachedCredential{Value: "stale", ExpiresAt: time.Now().Add(-1 * time.Minute)}

	if err := c.Put(ctx, "k1", tok1); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if err := c.Put(ctx, "k2", tok2); err != nil {
		t.Fatalf("Put k2: %v", err)
	}

	got1, found1, err := c.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get k1: %v", err)
	}
	if !found1 {
		t.Error("expected found=true for k1")
	}
	if got1.Value != "access-token-1" {
		t.Errorf("k1 Value: got %q, want %q", got1.Value, "access-token-1")
	}

	_, found2, err := c.Get(ctx, "k2")
	if err != nil {
		t.Fatalf("Get k2: %v", err)
	}
	if found2 {
		t.Error("expected found=false for stale k2")
	}
}

// T3: Delete removes an entry that was previously found.
func TestDelete_RemovesEntry(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, found, err := c.Get(ctx, "k")
	if err != nil || !found {
		t.Fatalf("pre-delete Get: found=%v err=%v", found, err)
	}

	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, found, err = c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("post-delete Get: %v", err)
	}
	if found {
		t.Error("expected found=false after Delete")
	}
}

// T4: Delete is idempotent — both on a never-stored key and on double-delete.
func TestDelete_Idempotent(t *testing.T) {
	t.Parallel()

	t.Run("never-stored key", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		if err := c.Delete(ctx, "never-stored"); err != nil {
			t.Errorf("Delete(never-stored): expected nil, got %v", err)
		}
	})

	t.Run("double delete", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(t, defaultCfg)
		ctx := context.Background()
		tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(10 * time.Minute)}
		if err := c.Put(ctx, "k", tok); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := c.Delete(ctx, "k"); err != nil {
			t.Errorf("first Delete: %v", err)
		}
		if err := c.Delete(ctx, "k"); err != nil {
			t.Errorf("second Delete: %v", err)
		}
	})
}

// T5: Concurrent access under -race flag.
// 50 goroutines Put, 50 goroutines Get — no data race allowed.
// t.Errorf (not t.Fatal) is used inside goroutines per contract.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 1000, ClockSkew: 0, MaxTTL: time.Hour})
	ctx := context.Background()

	const n = 50
	expiresAt := time.Now().Add(10 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(n * 2)

	// Writers
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			tok := CachedCredential{Value: fmt.Sprintf("val-%d", i), ExpiresAt: expiresAt}
			if err := c.Put(ctx, key, tok); err != nil {
				t.Errorf("Put(%s): %v", key, err)
			}
		}()
	}

	// Readers
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			tok, found, err := c.Get(ctx, key)
			if err != nil {
				t.Errorf("Get(%s): %v", key, err)
			}
			// found may be false if Put hasn't run yet — that's a valid race outcome.
			if found && tok.ExpiresAt.IsZero() {
				t.Errorf("Get(%s): found=true but ExpiresAt is zero", key)
			}
		}()
	}

	wg.Wait()
}

// T6: TTL normal case — token with ExpiresAt well beyond clockSkew is returned immediately.
func TestTTL_NormalCase(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: 24 * time.Hour})
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(2 * time.Minute)}
	if err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, found, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("expected found=true for token well within TTL")
	}
}

// T7: TTL MaxTTL cap — token with long ExpiresAt is capped to MaxTTL but still positive.
func TestTTL_MaxTTLCap(t *testing.T) {
	t.Parallel()
	// MaxTTL=1m, ClockSkew=30s → effective TTL = min(rawTTL-30s, 1m).
	// rawTTL ≈ 1h → safeTTL ≈ 59m30s → capped to 1m. Still positive → stored.
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: time.Minute})
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
	if err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, found, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("expected found=true after MaxTTL cap with positive remaining TTL")
	}
}

// T8: TTL within skew margin — token expiring within clockSkew window is not stored.
func TestTTL_WithinSkewMargin(t *testing.T) {
	t.Parallel()
	// ClockSkew=30s, token expires in 20s → safeTTL = 20s-30s = -10s → ttl<=0 → not stored.
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: 24 * time.Hour})
	ctx := context.Background()

	expiresAt := time.Now().Add(20 * time.Second)
	tok := CachedCredential{Value: "v", ExpiresAt: expiresAt}
	if err := c.Put(ctx, "k", tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, found, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expected found=false for token within clock skew margin")
	}
}

// T9: Freshness contract — cache miss and expired-token Put both return (CachedCredential{}, false, nil).
func TestFreshnessContract_Indistinguishable(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	// Sub-case A: key never stored.
	gotA, foundA, errA := c.Get(ctx, "never-stored")
	if errA != nil || foundA || gotA != (CachedCredential{}) {
		t.Errorf("sub-case A: got (%+v, %v, %v), want (CachedCredential{}, false, nil)", gotA, foundA, errA)
	}

	// Sub-case B: expired token dropped by Put (ttl<=0), Get sees absence.
	tokB := CachedCredential{Value: "old", ExpiresAt: time.Now().Add(-1 * time.Minute)}
	if err := c.Put(ctx, "k", tokB); err != nil {
		t.Fatalf("Put: %v", err)
	}
	gotB, foundB, errB := c.Get(ctx, "k")
	if errB != nil || foundB || gotB != (CachedCredential{}) {
		t.Errorf("sub-case B: got (%+v, %v, %v), want (CachedCredential{}, false, nil)", gotB, foundB, errB)
	}

	// Both results are structurally identical — callers cannot distinguish the two cases.
}

// T11: Put overwrites an existing key with a new value.
func TestPut_OverwritesExistingKey(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()
	exp := time.Now().Add(10 * time.Minute)

	if err := c.Put(ctx, "k", CachedCredential{Value: "token1", ExpiresAt: exp}); err != nil {
		t.Fatalf("Put token1: %v", err)
	}
	if err := c.Put(ctx, "k", CachedCredential{Value: "token2", ExpiresAt: exp}); err != nil {
		t.Fatalf("Put token2: %v", err)
	}

	got, found, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after overwrite")
	}
	if got.Value != "token2" {
		t.Errorf("Value: got %q, want %q", got.Value, "token2")
	}
}

// T12: MaxSize=0 panics at construction.
// otter.MustBuilder panics on invalid MaxSize (panic-on-misuse contract).
func TestNewTokenCache_PanicsOnMaxSizeZero(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for MaxSize=0, got none")
		}
	}()
	//nolint:errcheck // return value unreachable — panic fires inside MustBuilder.
	_, _ = NewTokenCache(CacheConfig{MaxSize: 0, ClockSkew: 0, MaxTTL: time.Hour})
}

// T14: All Put/Get/Delete calls return nil error, including edge cases.
func TestErrors_AlwaysNil(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	// Put with future ExpiresAt.
	if err := c.Put(ctx, "fresh", CachedCredential{Value: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Errorf("Put(fresh): %v", err)
	}

	// Put with past ExpiresAt (ttl<=0, silently dropped).
	if err := c.Put(ctx, "expired", CachedCredential{Value: "v", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Errorf("Put(expired): %v", err)
	}

	// Get existing (fresh) key.
	if _, _, err := c.Get(ctx, "fresh"); err != nil {
		t.Errorf("Get(fresh): %v", err)
	}

	// Get nonexistent key.
	if _, _, err := c.Get(ctx, "nonexistent"); err != nil {
		t.Errorf("Get(nonexistent): %v", err)
	}

	// Delete existing key.
	if err := c.Delete(ctx, "fresh"); err != nil {
		t.Errorf("Delete(fresh): %v", err)
	}

	// Delete nonexistent key.
	if err := c.Delete(ctx, "nonexistent"); err != nil {
		t.Errorf("Delete(nonexistent): %v", err)
	}
}
