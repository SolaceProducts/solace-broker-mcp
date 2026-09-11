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

	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache/cachetest"
)

func TestResolveTokenExpiryFallback_OmittedReturnsZero(t *testing.T) {
	t.Parallel()
	if got := resolveTokenExpiryFallback(nil); got != 0 {
		t.Errorf("resolveTokenExpiryFallback(nil) = %v, want 0", got)
	}
}

func TestResolveTokenExpiryFallback_ConfiguredPassesThrough(t *testing.T) {
	t.Parallel()
	if got := resolveTokenExpiryFallback(ptr(time.Hour)); got != time.Hour {
		t.Errorf("resolveTokenExpiryFallback(1h) = %v, want 1h", got)
	}
}

func TestFromConfig_TokenExpiryFallbackPlumbsThroughToExchanger(t *testing.T) {
	t.Parallel()
	cfg := validBrokerOAuthConfig()
	cfg.TokenExpiryFallback = ptr(time.Hour)

	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if e.tokenExpiryFallback != time.Hour {
		t.Errorf("tokenExpiryFallback = %v, want 1h", e.tokenExpiryFallback)
	}
}

func TestFromConfig_TokenExpiryFallbackOmittedPreservesDisabledSentinel(t *testing.T) {
	t.Parallel()
	cfg := validBrokerOAuthConfig()
	cfg.TokenExpiryFallback = nil

	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if e.tokenExpiryFallback != 0 {
		t.Errorf("tokenExpiryFallback = %v, want 0", e.tokenExpiryFallback)
	}
}
