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

// These tests pin buildToolPolicy's postcondition: gate enabled ⟺ returned
// *authz.Policy is non-nil. Main's fail-closed guard asserts the mirror
// direction; a break here fails these tests first.

// makeEnabledConfig returns a minimal ServerConfig whose gate resolves to true.
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

// Gate off (feature flag unset) → (nil, nil) regardless of config contents.
func TestBuildToolPolicy_GateOff_ReturnsNilNil(t *testing.T) {
	// Feature flag unset: gate is off even with a populated RBAC block.
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

// Gate on + valid block → non-nil Policy, no error.
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

// Non-OAuth mode with flag set → gate off, (nil, nil). Tool authorization
// only runs when identity is available in tokens.
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
