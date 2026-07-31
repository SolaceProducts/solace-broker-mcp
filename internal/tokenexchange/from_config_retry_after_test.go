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

package tokenexchange

import (
	"net/http"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache/cachetest"
)

func TestResolveMaxHonoredRetryAfter_NilBlockReturnsZero(t *testing.T) {
	t.Parallel()
	if got := resolveMaxHonoredRetryAfter(nil); got != 0 {
		t.Errorf("resolveMaxHonoredRetryAfter(nil) = %v, want 0", got)
	}
}

func TestResolveMaxHonoredRetryAfter_OmittedFieldReturnsZero(t *testing.T) {
	t.Parallel()
	if got := resolveMaxHonoredRetryAfter(&config.IdPRetryAfterConfig{}); got != 0 {
		t.Errorf("resolveMaxHonoredRetryAfter(empty block) = %v, want 0", got)
	}
}

func TestResolveMaxHonoredRetryAfter_SetFieldPassesThrough(t *testing.T) {
	t.Parallel()
	got := resolveMaxHonoredRetryAfter(&config.IdPRetryAfterConfig{MaxHonoredDuration: ptr(90 * time.Second)})
	if got != 90*time.Second {
		t.Errorf("resolveMaxHonoredRetryAfter(90s) = %v, want 90s", got)
	}
}

func TestFromConfig_RetryAfterOverridePlumbsThroughToExchanger(t *testing.T) {
	t.Parallel()
	cfg := validBrokerOAuthConfig()
	cfg.RetryAfter = &config.IdPRetryAfterConfig{MaxHonoredDuration: ptr(5 * time.Minute)}

	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	got, wasClamped := e.clampRetryAfter(2 * time.Minute)
	if wasClamped {
		t.Error("wasClamped = true for a value under the operator's configured cap, want false")
	}
	if got != 2*time.Minute {
		t.Errorf("clamped = %v, want unchanged 2m (operator's 5m cap, not the 60s shipped default)", got)
	}
}

func TestFromConfig_RetryAfterOmittedUsesShippedDefault(t *testing.T) {
	t.Parallel()
	cfg := validBrokerOAuthConfig()
	cfg.RetryAfter = nil

	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	got, wasClamped := e.clampRetryAfter(defaultMaxHonoredRetryAfter + time.Second)
	if !wasClamped {
		t.Error("wasClamped = false for a value over the shipped default cap, want true")
	}
	if got != defaultMaxHonoredRetryAfter {
		t.Errorf("clamped = %v, want the shipped default %v", got, defaultMaxHonoredRetryAfter)
	}
}
