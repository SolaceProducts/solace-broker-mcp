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
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

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

// When the whole tool_authorization block is omitted in oauth mode,
// applyDefaults synthesizes &ToolAuthorizationConfig{Enabled: nil} so the
// validator fires requiring enabled to be set explicitly.
func TestToolAuthorization_OmittedBlockSynthesizedInOAuthMode(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
` + oauthBaseYAML

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error when tool_authorization block is omitted in oauth mode")
	}
	const wantErr = `mcp_client_auth.tool_authorization.enabled must be set explicitly to true or false when mcp_client_auth.mode is "oauth"`
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("expected error %q, got: %v", wantErr, err)
	}
}

// enabled: false alone is a legal opt-out — no other fields required.
func TestToolAuthorization_EnabledFalseAloneIsLegal(t *testing.T) {
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
		t.Fatalf("enabled: false alone should be legal, got: %v", err)
	}
	if cfg.MCPClientAuth.ToolAuthorization == nil {
		t.Fatal("ToolAuthorization should not be nil")
	}
	if cfg.MCPClientAuth.ToolAuthorization.Enabled == nil || *cfg.MCPClientAuth.ToolAuthorization.Enabled {
		t.Error("Enabled should be non-nil and false")
	}
}

// enabled: false with populated fields loads; structural rules still apply.
func TestToolAuthorization_EnabledFalseWithPopulatedFieldsLoads(t *testing.T) {
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
		t.Fatalf("enabled: false with populated fields should load, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if ta == nil {
		t.Fatal("ToolAuthorization should not be nil")
	}
	if ta.GroupsClaimName == nil || *ta.GroupsClaimName != "roles" {
		t.Errorf("GroupsClaimName should be %q, got %v", "roles", ta.GroupsClaimName)
	}
	if len(ta.AccessLevelGroups) != 1 {
		t.Errorf("expected 1 group, got %d", len(ta.AccessLevelGroups))
	}
	if tools, ok := ta.AccessLevelGroups["Ops"]; !ok || len(tools) != 1 || tools[0] != "get-broker-status" {
		t.Errorf("expected Ops: [get-broker-status], got %v", ta.AccessLevelGroups)
	}
}

// Omitted groups_claim_name defaults to pointer-to-"groups".
func TestToolAuthorization_OmittedGroupsClaimNameDefaultsToGroups(t *testing.T) {
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
		t.Fatalf("should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if ta == nil {
		t.Fatal("ToolAuthorization should not be nil")
	}
	if ta.GroupsClaimName == nil {
		t.Fatal("GroupsClaimName should be non-nil (defaulted), got nil")
	}
	if *ta.GroupsClaimName != "groups" {
		t.Errorf("GroupsClaimName should default to %q, got %q", "groups", *ta.GroupsClaimName)
	}
}

// Explicit blank groups_claim_name values are rejected.
func TestToolAuthorization_BlankGroupsClaimNameRejected(t *testing.T) {
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
				t.Fatalf("expected error for groups_claim_name: %s", tc.value)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("expected error %q, got: %v", wantErr, err)
			}
		})
	}
}

// enabled: true with empty access_level_groups is rejected.
func TestToolAuthorization_EnabledTrueEmptyAccessLevelGroupsRejected(t *testing.T) {
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
		t.Fatal("expected error when enabled: true and access_level_groups is empty")
	}
	const wantErr = "mcp_client_auth.tool_authorization.access_level_groups is required when mcp_client_auth.tool_authorization.enabled is true"
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("expected error %q, got: %v", wantErr, err)
	}
}

// Case-sensitive group names are preserved through load.
func TestToolAuthorization_CaseSensitiveGroupNamesPreserved(t *testing.T) {
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
		t.Fatalf("should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if _, ok := ta.AccessLevelGroups["Ops"]; !ok {
		t.Error("expected key \"Ops\" in AccessLevelGroups")
	}
	if _, ok := ta.AccessLevelGroups["ops"]; !ok {
		t.Error("expected key \"ops\" in AccessLevelGroups")
	}
	if len(ta.AccessLevelGroups) != 2 {
		t.Errorf("expected 2 distinct groups, got %d", len(ta.AccessLevelGroups))
	}
}

// Group name with trailing whitespace is accepted verbatim.
func TestToolAuthorization_WhitespaceInGroupNamePreservedVerbatim(t *testing.T) {
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
		t.Fatalf("should load without error, got: %v", err)
	}
	ta := cfg.MCPClientAuth.ToolAuthorization
	if _, ok := ta.AccessLevelGroups["Ops "]; !ok {
		keys := make([]string, 0, len(ta.AccessLevelGroups))
		for k := range ta.AccessLevelGroups {
			keys = append(keys, k)
		}
		t.Errorf("expected key %q in AccessLevelGroups, got keys: %v", "Ops ", keys)
	}
}

// Empty and whitespace-only group names are rejected.
func TestToolAuthorization_EmptyAndWhitespaceOnlyGroupNamesRejected(t *testing.T) {
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
				t.Fatalf("expected error for group name %s", tc.groupKey)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("expected error %q, got: %v", wantErr, err)
			}
		})
	}
}

// Unclean tool names inside a group's list are rejected. The rule is: a
// tool name must be non-empty and equal to its trimmed form. This single
// invariant covers empty, whitespace-only, and surrounding-whitespace
// ("list-queues ") in one rule. Valid tool names pass; internal
// whitespace ("list queues") is intentionally accepted at this layer —
// it is a syntactically clean string, and unknown-tool detection is the
// registry check's job.
func TestToolAuthorization_UncleanToolNamesRejected(t *testing.T) {

	const wantErrTmpl = `mcp_client_auth.tool_authorization.access_level_groups: tool name at "Ops" [%s] must be non-empty and have no leading or trailing whitespace`

	cases := []struct {
		name      string
		toolsYAML string
		wantIdx   string // empty means expect no error
	}{
		{
			name: "empty string at index 0",
			toolsYAML: `        - ""
        - list-queues`,
			wantIdx: "0",
		},
		{
			name: "single space at index 1",
			toolsYAML: `        - get-broker-status
        - " "`,
			wantIdx: "1",
		},
		{
			name: "tab-only at index 0",
			toolsYAML: `        - "\t"
        - list-queues`,
			wantIdx: "0",
		},
		{
			name: "mixed spaces and tabs at index 1",
			toolsYAML: `        - get-broker-status
        - " \t "`,
			wantIdx: "1",
		},
		{
			name: "trailing space at index 0",
			toolsYAML: `        - "list-queues "
        - get-broker-status`,
			wantIdx: "0",
		},
		{
			name: "leading space at index 1",
			toolsYAML: `        - get-broker-status
        - " list-queues"`,
			wantIdx: "1",
		},
		{
			name: "valid tool names accepted",
			toolsYAML: `        - get-broker-status
        - list-queues`,
		},
		{
			name: "internal whitespace accepted at this layer",
			toolsYAML: `        - "list queues"
        - get-broker-status`,
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
    enabled: true
    access_level_groups:
      Ops:
` + tc.toolsYAML + `
` + oauthBaseYAML

			_, err := LoadConfig(writeTemp(t, yaml))
			if tc.wantIdx == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			wantErr := fmt.Sprintf(wantErrTmpl, tc.wantIdx)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", wantErr)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("expected error containing %q, got: %v", wantErr, err)
			}
		})
	}
}

// When a group has multiple unclean tool names, they collapse into one
// error line per group with all offending indices joined. Slice indices
// are stable so this stays deterministic (in contrast to the group-name
// check, which breaks on the first offender because map iteration is
// nondeterministic). Plural noun ("names") is used when more than one
// index is bad. Every group with offenders is reported (accumulation
// across groups is preserved).
func TestToolAuthorization_MultipleUncleanToolNamesCollapsedPerGroup(t *testing.T) {
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
        - ""
        - list-queues
        - " "
        - "list-queues "
      Admin:
        - get-broker-status
        - "\t"
` + oauthBaseYAML

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	// Ops has three bad indices (empty, whitespace-only, trailing space) → plural, joined.
	wantOps := `tool names at "Ops" [0, 2, 3] must be non-empty and have no leading or trailing whitespace`
	// Admin has one bad index → singular.
	wantAdmin := `tool name at "Admin" [1] must be non-empty and have no leading or trailing whitespace`
	if !strings.Contains(err.Error(), wantOps) {
		t.Errorf("expected error to contain %q, got: %v", wantOps, err)
	}
	if !strings.Contains(err.Error(), wantAdmin) {
		t.Errorf("expected error to contain %q, got: %v", wantAdmin, err)
	}
}

// LogValue renders enabled as "true" or "false" for configs that survive
// LoadConfig. The "unset" (nil) case cannot survive validation — it is covered
// by TestToolAuthorization_OAuthModeEnabledOmittedRejects.
func TestToolAuthorization_LogValueEnabledRendering(t *testing.T) {
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
				t.Fatalf("should load without error, got: %v", err)
			}

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

			out := buf.String()
			if !strings.Contains(out, tc.wantEnabled) {
				t.Errorf("expected %s in log output, got: %s", tc.wantEnabled, out)
			}
		})
	}
}

// LogValue renders groups_claim_name verbatim.
func TestToolAuthorization_LogValueGroupsClaimNameVerbatim(t *testing.T) {
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
		t.Fatalf("should load without error, got: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

	out := buf.String()
	if !strings.Contains(out, `"groups_claim_name":"custom_groups"`) {
		t.Errorf("expected groups_claim_name:custom_groups in log output, got: %s", out)
	}
}

// LogValue omits access_level_groups from the emitted slog record.
func TestToolAuthorization_LogValueOmitsAccessLevelGroups(t *testing.T) {
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
		t.Fatalf("should load without error, got: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", slog.Any("tool_authorization", cfg.MCPClientAuth.ToolAuthorization))

	out := buf.String()
	if strings.Contains(out, "access_level_groups") {
		t.Errorf("access_level_groups should be omitted from log output, got: %s", out)
	}
}

// tool_authorization under non-oauth modes is rejected.
func TestToolAuthorization_NonOAuthModeWithBlockRejects(t *testing.T) {
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
				t.Fatalf("expected error for mode %q with tool_authorization present", tc.mode)
			}
			wantErr := `mcp_client_auth.tool_authorization is only supported when mcp_client_auth.mode is "oauth" (currently: "` + tc.mode + `"); either set mode to "oauth" or remove the tool_authorization block`
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("expected error %q, got: %v", wantErr, err)
			}
		})
	}
}

// --- ToolAuthorizationEnabled ------------------------------------------------

func TestToolAuthorizationEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	cases := []struct {
		name string
		cfg  *ServerConfig
		want bool
	}{
		{
			name: "non-oauth mode returns false",
			cfg: &ServerConfig{
				MCPClientAuth: MCPClientAuthConfig{
					Mode: "static",
					ToolAuthorization: &ToolAuthorizationConfig{
						Enabled: &trueVal,
					},
				},
			},
			want: false,
		},
		{
			name: "nil ToolAuthorization returns false",
			cfg: &ServerConfig{
				MCPClientAuth: MCPClientAuthConfig{
					Mode:              AuthModeOAuth,
					ToolAuthorization: nil,
				},
			},
			want: false,
		},
		{
			name: "nil Enabled returns false",
			cfg: &ServerConfig{
				MCPClientAuth: MCPClientAuthConfig{
					Mode:              AuthModeOAuth,
					ToolAuthorization: &ToolAuthorizationConfig{Enabled: nil},
				},
			},
			want: false,
		},
		{
			name: "enabled false returns false",
			cfg: &ServerConfig{
				MCPClientAuth: MCPClientAuthConfig{
					Mode:              AuthModeOAuth,
					ToolAuthorization: &ToolAuthorizationConfig{Enabled: &falseVal},
				},
			},
			want: false,
		},
		{
			name: "oauth mode with enabled true returns true",
			cfg: &ServerConfig{
				MCPClientAuth: MCPClientAuthConfig{
					Mode:              AuthModeOAuth,
					ToolAuthorization: &ToolAuthorizationConfig{Enabled: &trueVal},
				},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToolAuthorizationEnabled(tc.cfg)
			if got != tc.want {
				t.Errorf("ToolAuthorizationEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// In oauth mode, omitting enabled from the block is rejected.
func TestToolAuthorization_OAuthModeEnabledOmittedRejects(t *testing.T) {
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
		t.Fatal("expected error when enabled is omitted in oauth mode")
	}
	const wantErr = `mcp_client_auth.tool_authorization.enabled must be set explicitly to true or false when mcp_client_auth.mode is "oauth"`
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("expected error %q, got: %v", wantErr, err)
	}
}

// TestToolAuthorization_FilterToolsListEnabled pins every input shape the helper
// can see: the flag true, false, omitted, and the whole block nil. Only an
// explicit true reads as on.
func TestToolAuthorization_FilterToolsListEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	cases := []struct {
		name string
		cfg  *ToolAuthorizationConfig
		want bool
	}{
		{
			name: "FilterToolsList set to true",
			cfg: &ToolAuthorizationConfig{
				FilterToolsList: &trueVal,
			},
			want: true,
		},
		{
			name: "FilterToolsList set to false",
			cfg: &ToolAuthorizationConfig{
				FilterToolsList: &falseVal,
			},
			want: false,
		},
		{
			name: "FilterToolsList is omitted",
			cfg: &ToolAuthorizationConfig{
				FilterToolsList: nil,
			},
			want: false,
		},
		{
			// The shape in every non-oauth deployment, where the whole
			// tool_authorization block is absent.
			name: "whole block is nil",
			cfg:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterToolsListEnabled(tc.cfg)
			if got != tc.want {
				t.Errorf("FilterToolsListEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
