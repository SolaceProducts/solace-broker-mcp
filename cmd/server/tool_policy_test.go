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

package main

import (
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// These tests pin the buildToolPolicy postcondition that main relies on:
// tool authorization enabled ⟺ returned *authz.Policy is non-nil. That
// biconditional is what lets RegisterWithServer treat nil-policy as
// "skip the wrapper" without ambiguity — the invariant is a property of
// this function, not folklore scattered across the composition site.
// The mirror direction (gate on ⟹ policy non-nil) is asserted by main
// itself via a fail-closed guard immediately after the call; if a future
// refactor accidentally makes buildToolPolicy return (nil, nil) on the
// gate-on branch, this test suite fails first, and if this suite is
// bypassed the guard aborts startup before the server accepts requests.

// makeEnabledConfig returns a minimal ServerConfig whose gate resolves
// to true. Uses the smallest set of fields both ToolAuthorizationEnabled
// and NewPolicy actually read.
func makeEnabledConfig() *config.ServerConfig {
	enabled := true
	return &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode: config.AuthModeOAuth,
			ToolAuthorization: &config.ToolAuthorizationConfig{
				Enabled: &enabled,
				AccessLevelGroups: map[string][]string{
					"Ops": {"get-broker-status"},
				},
			},
		},
	}
}

// TestBuildToolPolicy_GateOff_ReturnsNilNil pins the disabled-deployment
// side of the biconditional: when the feature flag is absent, the gate
// short-circuits to false regardless of config shape, and buildToolPolicy
// must return (nil, nil). That nil is the signal RegisterWithServer keys
// off to skip the authorization wrapper.
func TestBuildToolPolicy_GateOff_ReturnsNilNil(t *testing.T) {
	// Feature flag not set: gate is off even with a fully-populated block.
	t.Setenv("ENABLE_TOOL_AUTHORIZATION", "")
	cfg := makeEnabledConfig()

	policy, err := buildToolPolicy(cfg)
	if err != nil {
		t.Fatalf("buildToolPolicy on disabled gate: unexpected error: %v", err)
	}
	if policy != nil {
		t.Fatalf("buildToolPolicy on disabled gate: got non-nil policy, want nil")
	}
}

// TestBuildToolPolicy_GateOn_ReturnsNonNilPolicy pins the enabled-
// deployment side: with the feature flag set and a valid RBAC block,
// the gate is true and NewPolicy succeeds — buildToolPolicy returns a
// non-nil compiled Policy and no error. This is the postcondition
// RegisterWithServer relies on to compose the authorization wrapper.
func TestBuildToolPolicy_GateOn_ReturnsNonNilPolicy(t *testing.T) {
	t.Setenv("ENABLE_TOOL_AUTHORIZATION", "true")
	cfg := makeEnabledConfig()

	policy, err := buildToolPolicy(cfg)
	if err != nil {
		t.Fatalf("buildToolPolicy on enabled gate: unexpected error: %v", err)
	}
	if policy == nil {
		t.Fatalf("buildToolPolicy on enabled gate: got nil policy, want non-nil")
	}
}

// TestBuildToolPolicy_NonOAuthMode_GateFalse pins that the gate stays
// off in non-OAuth deployments even when the feature flag is set —
// tool authorization only runs when identity is available in tokens.
// buildToolPolicy therefore returns (nil, nil), matching the gate.
func TestBuildToolPolicy_NonOAuthMode_GateFalse(t *testing.T) {
	t.Setenv("ENABLE_TOOL_AUTHORIZATION", "true")
	cfg := makeEnabledConfig()
	cfg.MCPClientAuth.Mode = config.AuthModeStatic

	policy, err := buildToolPolicy(cfg)
	if err != nil {
		t.Fatalf("buildToolPolicy in non-OAuth mode: unexpected error: %v", err)
	}
	if policy != nil {
		t.Fatalf("buildToolPolicy in non-OAuth mode: got non-nil policy, want nil")
	}
}
