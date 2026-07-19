package cache

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	MaxSize:   100,
	ClockSkew: 0,
	MaxTTL:    time.Hour,
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
		if res.Status != GetMissAbsent {
			t.Errorf("expected GetMissAbsent, got %v", res.Status)
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
	if res.Status != GetMissAbsent {
		t.Error("expected GetMissAbsent after Delete")
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
	c := newTestCache(t, CacheConfig{MaxSize: 1000, ClockSkew: 0, MaxTTL: time.Hour})
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

// T6: TTL normal case — token with ExpiresAt well beyond clockSkew is returned immediately.
func TestTTL_NormalCase(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: 24 * time.Hour})
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
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: time.Minute})
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

// T8: TTL within skew margin — token expiring within clockSkew window is not stored.
func TestTTL_WithinSkewMargin(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 30 * time.Second, MaxTTL: 24 * time.Hour})
	ctx := context.Background()

	tok := CachedCredential{Value: "v", ExpiresAt: time.Now().Add(20 * time.Second)}
	pr, err := c.Put(ctx, "k", tok)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if pr.Status != PutDroppedTTL {
		t.Errorf("expected PutDroppedTTL, got %v", pr.Status)
	}

	res, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetMissAbsent {
		t.Error("expected GetMissAbsent for token within clock skew margin")
	}
}

// T9: GetMissAbsent is returned for a never-stored key.
// GetMissExpired exists for belt-and-suspenders (Otter sweeper lag) but is not
// deterministically testable — Otter evicts before ExpiresAt is reached.
func TestGetResult_AbsentStatus(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, defaultCfg)
	ctx := context.Background()

	res, err := c.Get(ctx, "never-stored")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != GetMissAbsent {
		t.Errorf("got %v, want GetMissAbsent", res.Status)
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
	_, err := NewTokenCache(CacheConfig{MaxSize: 0, ClockSkew: 0, MaxTTL: time.Hour})
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
	_, err := NewTokenCache(CacheConfig{MaxSize: 100, ClockSkew: 0, MaxTTL: 0})
	if err == nil {
		t.Fatal("expected error for MaxTTL=0, got nil")
	}
}

func TestNewTokenCache_ErrorOnMaxTTLNegative(t *testing.T) {
	t.Parallel()
	_, err := NewTokenCache(CacheConfig{MaxSize: 100, ClockSkew: 0, MaxTTL: -time.Second})
	if err == nil {
		t.Fatal("expected error for negative MaxTTL, got nil")
	}
}

// T12b: A negative ClockSkew is a wiring bug — it would extend the effective
// TTL past the token's real ExpiresAt, serving already-expired credentials
// after Otter's sweeper hasn't caught up yet.
func TestNewTokenCache_ErrorOnClockSkewNegative(t *testing.T) {
	t.Parallel()
	_, err := NewTokenCache(CacheConfig{MaxSize: 100, ClockSkew: -time.Second, MaxTTL: time.Hour})
	if err == nil {
		t.Fatal("expected error for negative ClockSkew, got nil")
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

// TestPutResult_LogValueEmitsStatusOnly pins the symmetric guard for
// PutResult. Today PutResult only carries Status so no token can leak,
// but pinning the shape now catches a regression if a future backend
// grows the type with fields that should not be blanket-reflected.
func TestPutResult_LogValueEmitsStatusOnly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))

	pr := PutResult{Status: PutDroppedFull}
	l.LogAttrs(context.Background(), slog.LevelInfo, "test", slog.Any("cache_result", pr))

	out := buf.String()
	if !strings.Contains(out, `"status":"dropped_full"`) {
		t.Errorf("PutResult log should surface status=dropped_full; got: %s", out)
	}
}

// TestNewTokenCache_ValidationFirstFailureWins pins the caller-observable
// contract that when multiple CacheConfig fields are invalid, the constructor
// reports only the FIRST failing field, in the fixed order MaxSize → MaxTTL →
// ClockSkew. Callers rely on this to fix misconfiguration one field at a time
// without the error message swimming around as they patch each field.
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
			name:      "all three invalid, MaxSize reported first",
			cfg:       CacheConfig{MaxSize: 0, MaxTTL: 0, ClockSkew: -time.Second},
			wantField: "MaxSize",
		},
		{
			name:      "MaxTTL and ClockSkew invalid, MaxTTL reported first",
			cfg:       CacheConfig{MaxSize: 100, MaxTTL: 0, ClockSkew: -time.Second},
			wantField: "MaxTTL",
		},
		{
			name:      "only ClockSkew invalid, ClockSkew reported",
			cfg:       CacheConfig{MaxSize: 100, MaxTTL: time.Hour, ClockSkew: -time.Second},
			wantField: "ClockSkew",
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
	c := newTestCache(t, CacheConfig{MaxSize: 100, ClockSkew: 0, MaxTTL: time.Hour})
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
	c := newTestCache(t, CacheConfig{MaxSize: 1000, ClockSkew: 0, MaxTTL: time.Hour})
	ctx := context.Background()

	const n = 50
	expiresAt := time.Now().Add(10 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(n * 3)

	// Overlap keys across all three operation types by using key-<i%n>.
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

// TestGetStatus_Level pins the slog level mapping the token exchanger consumes
// at exchange.go:41. GetHit and GetMissAbsent are Debug (routine cache
// traffic); GetMissExpired is Warn (a stale entry survived until Get — worth
// noticing).
func TestGetStatus_Level(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status GetStatus
		want   slog.Level
	}{
		{"hit is debug", GetHit, slog.LevelDebug},
		{"miss_absent is debug", GetMissAbsent, slog.LevelDebug},
		{"miss_expired is warn", GetMissExpired, slog.LevelWarn},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.status.Level(); got != tc.want {
				t.Errorf("%v.Level(): got %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestPutStatus_Level pins the slog level mapping the token exchanger consumes
// at exchange.go:80. PutStored is Debug (routine); PutDroppedTTL and
// PutDroppedFull are Warn (the caller asked us to cache something we couldn't
// keep — worth surfacing).
func TestPutStatus_Level(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status PutStatus
		want   slog.Level
	}{
		{"stored is debug", PutStored, slog.LevelDebug},
		{"dropped_ttl is warn", PutDroppedTTL, slog.LevelWarn},
		{"dropped_full is warn", PutDroppedFull, slog.LevelWarn},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.status.Level(); got != tc.want {
				t.Errorf("%v.Level(): got %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
