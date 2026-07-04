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

// GetStatus describes the outcome of a Get call.
type GetStatus int

const (
	GetHit         GetStatus = iota // Entry found and fresh.
	GetMissAbsent                   // No entry for this key.
	GetMissExpired                  // Entry exists but failed the freshness check.
)

// GetResult is returned by Get. Callers inspect Status to distinguish
// hit, clean miss, and expired miss — each has different diagnostic meaning.
type GetResult struct {
	Entry  CachedCredential
	Status GetStatus
}

// PutStatus describes the outcome of a Put call.
type PutStatus int

const (
	PutStored     PutStatus = iota // Entry accepted and stored.
	PutDroppedTTL                  // TTL was zero or negative after clock-skew; not stored.
	PutDroppedFull                 // Otter's admission policy rejected the entry (cache full).
)

// PutResult is returned by Put.
type PutResult struct {
	Status PutStatus
}

// DeleteResult is returned by Delete. Empty today; exists so a future backend
// (e.g. Redis DEL returning a count) can surface info without changing the
// interface signature.
type DeleteResult struct{}

// TokenCache is the boundary between the Exchanger and whatever storage
// implementation backs the cache. The Exchanger depends on this interface only.
//
// Error contract: errors are for backend failures only (e.g., Redis down in a
// future implementation). In-memory implementations should never return
// non-nil errors.
type TokenCache interface {
	Get(ctx context.Context, key string) (GetResult, error)
	Put(ctx context.Context, key string, entry CachedCredential) (PutResult, error)
	Delete(ctx context.Context, key string) (DeleteResult, error)
}
