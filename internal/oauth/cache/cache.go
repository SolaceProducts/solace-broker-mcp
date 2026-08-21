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

// CacheConfig holds the parameters for constructing a TokenCache.
type CacheConfig struct {
	MaxSize   int
	ClockSkew time.Duration
	MaxTTL    time.Duration
}

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
	GetHit  GetStatus = iota // Entry found and fresh.
	GetMiss                  // No fresh entry available for this key.
)

func (s GetStatus) String() string {
	switch s {
	case GetHit:
		return "hit"
	case GetMiss:
		return "miss"
	default:
		return fmt.Sprintf("GetStatus(%d)", int(s))
	}
}

func (s GetStatus) Level() slog.Level {
	return slog.LevelDebug
}

// LogValue renders the status as its name ("hit"/"miss") rather than the
// underlying iota. slog resolves LogValuer but never calls String, so without
// this a caller logging a bare GetStatus emits an integer — and 1 ("miss")
// reads as a failure code next to the HTTP statuses logged elsewhere in the
// tree. Defined on the enum, not just its GetResult wrapper, so every call
// site is covered whether it logs the result or the status alone.
func (s GetStatus) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// GetResult is returned by Get. Callers inspect Status to distinguish
// a hit from a miss.
type GetResult struct {
	Entry  CachedCredential
	Status GetStatus
}

// LogValue keeps GetResult log-safe when passed to slog.Any. Without it,
// slog reflects into the struct and would emit Entry.Value (the raw
// credential) verbatim, bypassing CachedCredential's own LogValue guard.
// Only Status is exposed here; Entry stays out of every log record.
func (r GetResult) LogValue() slog.Value {
	return slog.GroupValue(slog.String("status", r.Status.String()))
}

// PutStatus describes the outcome of a Put call.
type PutStatus int

const (
	PutStored    PutStatus = iota // Entry accepted and stored.
	PutDroppedTTL                 // TTL was zero or negative after clock-skew; not stored.
)

func (s PutStatus) String() string {
	switch s {
	case PutStored:
		return "stored"
	case PutDroppedTTL:
		return "dropped_ttl"
	default:
		return fmt.Sprintf("PutStatus(%d)", int(s))
	}
}

func (s PutStatus) Level() slog.Level {
	switch s {
	case PutDroppedTTL:
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// LogValue is symmetric with GetStatus.LogValue. PutStatus is the more
// misleading of the two as a bare integer: PutStored is 0, which reads as a
// success exit code, while GetHit is also 0 — so the same number means
// "success" on both a get and a put line while the meaning of 1 flips between
// them.
func (s PutStatus) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// PutResult is returned by Put.
type PutResult struct {
	Status PutStatus
}

// LogValue is symmetric with GetResult.LogValue. PutResult carries no
// credential material today, but future backends may surface additional
// metadata (bytes written, admission decisions) that should not be
// blanket-reflected into log records. Pin the shape now.
func (r PutResult) LogValue() slog.Value {
	return slog.GroupValue(slog.String("status", r.Status.String()))
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
	Close() error
}
