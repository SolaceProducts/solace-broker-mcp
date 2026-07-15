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

package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// enableToolAuthorizationFlag sets ENABLE_TOOL_AUTHORIZATION=true for the
// duration of the test so the feature-gated validation and defaults fire.
func enableToolAuthorizationFlag(t *testing.T) {
	t.Helper()
	t.Setenv(envEnableToolAuthorization, "true")
}

// oauthBaseYAML is the minimal valid oauth scaffold shared by tool_authorization
// tests. Every test appends its tool_authorization block under mcp_client_auth.
const oauthBaseYAML = `
brokers:
  dev:
    url: "https://broker.example.com:943"
    auth:
      mode: basic
      username: admin
      password: secret
tls_terminated_upstream: true
`

// Q1: When the whole tool_authorization block is omitted in oauth mode,
// applyDefaults synthesizes &ToolAuthorizationConfig{Enabled: nil} so the
// I3 validator arm fires with the exact Q18 error message.
func TestToolAuthorization_Q1_OmittedBlockSynthesizedInOAuthMode(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
` + oauthBaseYAML

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected I3 error when tool_authorization block is omitted in oauth mode")
	}
	const wantErr = `mcp_client_auth.tool_authorization.enabled must be set explicitly to true or false when mcp_client_auth.mode is "oauth"`
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Q1: expected I3 error %q, got: %v", wantErr, err)
	}
}

// Q2: enabled: false alone is a legal opt-out — no other fields required.
func TestToolAuthorization_Q2_EnabledFalseAloneIsLegal(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q2: enabled: false alone should be legal, got: %v", err)
	}
	if cfg.MCPClientAuth.ToolAuthorization == nil {
		t.Fatal("Q2: ToolAuthorization should not be nil")
	}
	if cfg.MCPClientAuth.ToolAuthorization.Enabled == nil || *cfg.MCPClientAuth.ToolAuthorization.Enabled {
		t.Error("Q2: Enabled should be non-nil and false")
	}
}

// Q2: enabled: false with populated fields loads; structural rules still apply.
func TestToolAuthorization_Q2_EnabledFalseWithPopulatedFieldsLoads(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
    groups_claim_name: "roles"
    access_level_groups:
      Ops:
        - get-broker-status
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q2: enabled: false with populated fields should load, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if ta == nil {
		t.Fatal("Q2: ToolAuthorization should not be nil")
	}
	if ta.GroupsClaimName == nil || *ta.GroupsClaimName != "roles" {
		t.Errorf("Q2: GroupsClaimName should be %q, got %v", "roles", ta.GroupsClaimName)
	}
	if len(ta.AccessLevelGroups) != 1 {
		t.Errorf("Q2: expected 1 group, got %d", len(ta.AccessLevelGroups))
	}
	if tools, ok := ta.AccessLevelGroups["Ops"]; !ok || len(tools) != 1 || tools[0] != "get-broker-status" {
		t.Errorf("Q2: expected Ops: [get-broker-status], got %v", ta.AccessLevelGroups)
	}
}

// Q3: Omitted groups_claim_name defaults to pointer-to-"groups".
func TestToolAuthorization_Q3_OmittedGroupsClaimNameDefaultsToGroups(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q3: should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if ta == nil {
		t.Fatal("Q3: ToolAuthorization should not be nil")
	}
	if ta.GroupsClaimName == nil {
		t.Fatal("Q3: GroupsClaimName should be non-nil (defaulted), got nil")
	}
	if *ta.GroupsClaimName != "groups" {
		t.Errorf("Q3: GroupsClaimName should default to %q, got %q", "groups", *ta.GroupsClaimName)
	}
}

// Q3a: Explicit blank groups_claim_name values are rejected with the exact
// docstring-pinned error text.
func TestToolAuthorization_Q3a_BlankGroupsClaimNameRejected(t *testing.T) {
	enableToolAuthorizationFlag(t)
	const wantErr = `mcp_client_auth.tool_authorization.groups_claim_name must be a non-blank string when set; omit the field to accept the default ("groups")`

	cases := []struct {
		name  string
		value string
	}{
		{"empty string", `""`},
		{"single space", `" "`},
		{"tab", `"\t"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
    groups_claim_name: ` + tc.value + `
` + oauthBaseYAML

			_, err := LoadConfig(writeTemp(t, yaml))
			if err == nil {
				t.Fatalf("Q3a: expected error for groups_claim_name: %s", tc.value)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("Q3a: expected error %q, got: %v", wantErr, err)
			}
		})
	}
}

// Q4: enabled: true with empty access_level_groups is rejected.
func TestToolAuthorization_Q4_EnabledTrueEmptyAccessLevelGroupsRejected(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
` + oauthBaseYAML

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("Q4: expected error when enabled: true and access_level_groups is empty")
	}
	const wantErr = "mcp_client_auth.tool_authorization.access_level_groups is required when mcp_client_auth.tool_authorization.enabled is true"
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Q4: expected error %q, got: %v", wantErr, err)
	}
}

// Q5: Case-sensitive group names are preserved through load.
func TestToolAuthorization_Q5_CaseSensitiveGroupNamesPreserved(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
    access_level_groups:
      Ops:
        - get-broker-status
      ops:
        - list-queues
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q5: should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if _, ok := ta.AccessLevelGroups["Ops"]; !ok {
		t.Error("Q5: expected key \"Ops\" in AccessLevelGroups")
	}
	if _, ok := ta.AccessLevelGroups["ops"]; !ok {
		t.Error("Q5: expected key \"ops\" in AccessLevelGroups")
	}
	if len(ta.AccessLevelGroups) != 2 {
		t.Errorf("Q5: expected 2 distinct groups, got %d", len(ta.AccessLevelGroups))
	}
}

// Q6: Group name with trailing whitespace is accepted verbatim.
func TestToolAuthorization_Q6_WhitespaceInGroupNamePreservedVerbatim(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
    access_level_groups:
      "Ops ":
        - get-broker-status
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q6: should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if _, ok := ta.AccessLevelGroups["Ops "]; !ok {
		keys := make([]string, 0, len(ta.AccessLevelGroups))
		for k := range ta.AccessLevelGroups {
			keys = append(keys, k)
		}
		t.Errorf("Q6: expected key %q in AccessLevelGroups, got keys: %v", "Ops ", keys)
	}
}

// Q7: Empty and whitespace-only group names are rejected with the exact
// docstring-pinned error text.
func TestToolAuthorization_Q7_EmptyAndWhitespaceOnlyGroupNamesRejected(t *testing.T) {
	enableToolAuthorizationFlag(t)
	const wantErr = "mcp_client_auth.tool_authorization.access_level_groups: group name cannot be empty or whitespace-only"

	cases := []struct {
		name     string
		groupKey string
	}{
		{"empty string", `""`},
		{"single space", `" "`},
		{"tab", `"\t"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
    access_level_groups:
      ` + tc.groupKey + `:
        - get-broker-status
` + oauthBaseYAML

			_, err := LoadConfig(writeTemp(t, yaml))
			if err == nil {
				t.Fatalf("Q7: expected error for group name %s", tc.groupKey)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("Q7: expected error %q, got: %v", wantErr, err)
			}
		})
	}
}

// Q10: LogValue renders enabled as "true" or "false" for configs that survive
// LoadConfig. The "unset" (nil) case cannot survive validation — it is covered
// by Q17/Q18.
func TestToolAuthorization_Q10_LogValueEnabledThreeWay(t *testing.T) {
	enableToolAuthorizationFlag(t)
	cases := []struct {
		name        string
		enabledYAML string
		extraYAML   string
		wantEnabled string
	}{
		{
			name:        "enabled true",
			enabledYAML: "true",
			extraYAML: `    access_level_groups:
      Ops:
        - get-broker-status`,
			wantEnabled: `"enabled":"true"`,
		},
		{
			name:        "enabled false",
			enabledYAML: "false",
			extraYAML:   "",
			wantEnabled: `"enabled":"false"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: ` + tc.enabledYAML + `
` + tc.extraYAML + `
` + oauthBaseYAML

			cfg, err := LoadConfig(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("Q10: should load without error, got: %v", err)
			}

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

			out := buf.String()
			if !strings.Contains(out, tc.wantEnabled) {
				t.Errorf("Q10: expected %s in log output, got: %s", tc.wantEnabled, out)
			}
		})
	}
}

// Q11: LogValue renders groups_claim_name verbatim.
func TestToolAuthorization_Q11_LogValueGroupsClaimNameVerbatim(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
    groups_claim_name: "custom_groups"
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q11: should load without error, got: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

	out := buf.String()
	if !strings.Contains(out, `"groups_claim_name":"custom_groups"`) {
		t.Errorf("Q11: expected groups_claim_name:custom_groups in log output, got: %s", out)
	}
}

// Q12: LogValue omits access_level_groups from the emitted slog record.
func TestToolAuthorization_Q12_LogValueOmitsAccessLevelGroups(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
    access_level_groups:
      Ops:
        - get-broker-status
` + oauthBaseYAML

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Q12: should load without error, got: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

	out := buf.String()
	if strings.Contains(out, "access_level_groups") {
		t.Errorf("Q12: access_level_groups should be omitted from log output, got: %s", out)
	}
}

// Q14/Q15: I1 fires when tool_authorization is present under non-oauth modes.
// Error text is identical across modes with the current mode interpolated.
func TestToolAuthorization_Q14_Q15_I1_NonOAuthModeWithBlockRejects(t *testing.T) {
	enableToolAuthorizationFlag(t)
	cases := []struct {
		name     string
		mode     string
		authLine string
	}{
		{
			name:     "static mode",
			mode:     "static",
			authLine: "  dev_token: test",
		},
		{
			name:     "disabled mode",
			mode:     "disabled",
			authLine: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
mcp_client_auth:
  mode: ` + tc.mode + `
` + tc.authLine + `
  tool_authorization:
    enabled: true
    access_level_groups:
      Ops:
        - get-broker-status
brokers:
  dev:
    url: "https://broker.example.com:943"
    auth:
      mode: basic
      username: admin
      password: secret
`
			_, err := LoadConfig(writeTemp(t, yaml))
			if err == nil {
				t.Fatalf("Q14/Q15: expected I1 error for mode %q with tool_authorization present", tc.mode)
			}
			wantErr := `mcp_client_auth.tool_authorization is only supported when mcp_client_auth.mode is "oauth" (currently: "` + tc.mode + `"); either set mode to "oauth" or remove the tool_authorization block`
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("Q14/Q15: expected I1 error %q, got: %v", wantErr, err)
			}
		})
	}
}

// Q17/Q18: I3 fires when oauth mode + enabled is omitted from the block.
func TestToolAuthorization_Q17_Q18_I3_OAuthModeEnabledOmittedRejects(t *testing.T) {
	enableToolAuthorizationFlag(t)
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization: {}
` + oauthBaseYAML

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("Q17/Q18: expected I3 error when enabled is omitted in oauth mode")
	}
	const wantErr = `mcp_client_auth.tool_authorization.enabled must be set explicitly to true or false when mcp_client_auth.mode is "oauth"`
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Q17/Q18: expected I3 error %q, got: %v", wantErr, err)
	}
}
