// Package cache provides the TokenCache interface and its Otter v2-backed
// implementation for the OAuth token exchange feature (SOL-150052).
//
// The cache is silent: it emits no logs and no metrics. Observability for
// cache operations lives in the Exchanger (the caller), not here.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// CachedCredential is a cached OAuth access token with its expiry time as
// reported by the identity provider. The Value field must never appear in
// error messages, log lines, or metrics labels — it is a credential.
type CachedCredential struct {
	Value     string
	ExpiresAt time.Time
}

func (c CachedCredential) String() string {
	return fmt.Sprintf("CachedCredential{ExpiresAt: %v}", c.ExpiresAt)
}

func (c CachedCredential) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expires_at", c.ExpiresAt))
}

func (c CachedCredential) GoString() string {
	return c.String()
}

// TokenCache is the boundary between the Exchanger and whatever storage
// implementation backs the cache. The Exchanger depends on this interface only.
//
// Freshness contract: Get returns found=false for both "never stored" and
// "stored but expired" — callers cannot distinguish the two cases, by design.
// This keeps all freshness policy inside the cache implementation.
//
// Error contract: errors are for backend failures only (e.g., Redis down in a
// future implementation). In-memory implementations should never return
// non-nil errors.
type TokenCache interface {
	// Get returns a credential only if one is stored AND still fresh by the
	// cache's policy. found=false means "no usable entry here".
	Get(ctx context.Context, key string) (entry CachedCredential, found bool, err error)

	// Put stores a credential. The cache uses entry.ExpiresAt to determine TTL.
	// If the computed TTL is zero or negative (already expired or within the
	// clock-skew window), the entry is not stored.
	Put(ctx context.Context, key string, entry CachedCredential) error

	// Delete forcibly evicts a key. Idempotent — deleting a non-existent key
	// returns nil. Used for known-bad token paths (e.g., broker 401).
	Delete(ctx context.Context, key string) error
}
