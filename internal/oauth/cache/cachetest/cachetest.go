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

// Package cachetest provides *testing.T-aware constructors for TokenCache
// so tests always get a fresh cache with automatic Close on cleanup, and
// never leak Otter's background sweeper goroutine (which also pins the
// cache in memory as a GC root).
//
// Prefer these over calling cache.NewTokenCache directly in tests. The
// bare constructor allocates an Otter cache that will leak — a per-test
// goroutine and its retained heap — unless the caller manually plumbs
// Close through t.Cleanup.
//
// The cache package's own tests cannot import this sub-package (that
// would be a cycle) and use a local newTestCache helper instead.
package cachetest

import (
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache"
)

// defaultTestConfig is the sensible starting point for most tokenexchange /
// integration tests: small MaxSize so full-cache paths are cheap to exercise,
// zero ClockSkew so tests do not need to reason about the safety margin, and
// a 1-hour MaxTTL that covers any test-generated ExpiresAt.
var defaultTestConfig = cache.CacheConfig{
	MaxSize:   100,
	ClockSkew: 0,
	MaxTTL:    time.Hour,
}

// Default returns a fresh TokenCache with test-friendly defaults and
// registers t.Cleanup to Close it, so the sweeper goroutine dies with the
// test and the cache's heap becomes GC-eligible.
//
// Use this instead of building a cache.NewTokenCache directly in test code.
func Default(t *testing.T) cache.TokenCache {
	t.Helper()
	return WithConfig(t, defaultTestConfig)
}

// WithConfig is like Default but exposes CacheConfig so tests can exercise
// specific MaxSize / TTL / skew behavior. Prefer Default when the config
// does not matter to the test's assertions.
func WithConfig(t *testing.T, cfg cache.CacheConfig) cache.TokenCache {
	t.Helper()
	tc, err := cache.NewTokenCache(cfg)
	if err != nil {
		t.Fatalf("cache.NewTokenCache: %v", err)
	}
	t.Cleanup(func() {
		if err := tc.Close(); err != nil {
			t.Logf("cache.Close: %v", err)
		}
	})
	return tc
}
