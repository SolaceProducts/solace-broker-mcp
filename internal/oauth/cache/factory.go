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

import "fmt"

// NewTokenCache constructs a TokenCache backed by Otter v2 using the provided
// configuration. This is the only public constructor — no other package in the
// project instantiates the cache directly.
//
// All three CacheConfig fields are validated. MaxSize must be > 0; MaxTTL must
// be > 0; ClockSkew must be >= 0. A misconfigured MaxTTL of 0 (or negative)
// would silently make every Put drop as PutDroppedTTL, effectively disabling
// the cache — failing loud here surfaces the misconfiguration at startup
// rather than as a mysterious performance regression under load.
func NewTokenCache(cfg CacheConfig) (TokenCache, error) {
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("cache: MaxSize must be > 0, got %d", cfg.MaxSize)
	}
	if cfg.MaxTTL <= 0 {
		return nil, fmt.Errorf("cache: MaxTTL must be > 0, got %v", cfg.MaxTTL)
	}
	if cfg.ClockSkew < 0 {
		return nil, fmt.Errorf("cache: ClockSkew must be >= 0, got %v", cfg.ClockSkew)
	}
	return newOtterTokenCache(cfg)
}
