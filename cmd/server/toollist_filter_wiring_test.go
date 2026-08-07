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
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests pin the install gate: the tools/list filter runs only when a
// policy exists AND filter_tools_list is true, and the startup log states which
// of the three postures applies.
//
// Assertions are on the log rather than on the server's middleware chain, which
// the SDK keeps unexported. That is not a workaround — the startup line IS the
// contract here. An operator who sees fewer tools than expected has nothing
// else to consult, since the filter tells the caller nothing. WithListFiltering's
// own tests cover what happens once it is installed.

// captureStartupLog swaps slog.Default for a JSON handler writing to buf, at
// DEBUG so no posture line can be filtered out by level.
func captureStartupLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf, func() { slog.SetDefault(prev) }
}

const (
	postureEnabled          = "tools/list filtering is enabled"
	postureDisabled         = "tools/list filtering is disabled"
	postureReasonAuthzOff   = "tool_authorization.enabled is false"
	postureReasonFlagNotSet = "filter_tools_list not set"
)

// findPostureLine returns the single record whose msg mentions tools/list
// filtering, failing if there is not exactly one. Exactly one is the contract:
// the posture must be stated on every boot, and stated once.
func findPostureLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		msg, _ := entry["msg"].(string)
		if strings.Contains(msg, "tools/list filtering") {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d tools/list filtering posture lines, want exactly 1:\n%s",
			len(found), buf.String())
	}
	return found[0]
}

// assertPosture checks the message fragment and level of a posture line.
func assertPosture(t *testing.T, rec map[string]any, wantFragment, wantLevel string) {
	t.Helper()
	msg, _ := rec["msg"].(string)
	if !strings.Contains(msg, wantFragment) {
		t.Errorf("posture message %q does not contain %q", msg, wantFragment)
	}
	if got := rec["level"]; got != wantLevel {
		t.Errorf("posture level = %v, want %q; message was %q", got, wantLevel, msg)
	}
}

// assertReason checks the structured reason field, which is what distinguishes
// the two disabled postures from each other.
func assertReason(t *testing.T, rec map[string]any, want string) {
	t.Helper()
	if got := rec["reason"]; got != want {
		t.Errorf("reason = %v, want %q", got, want)
	}
}

// newTestServer builds a bare server to install middleware onto.
func newTestServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
}

// policyFrom compiles the policy the gate would receive for cfg.
func policyFrom(t *testing.T, cfg *config.ServerConfig) *authz.Policy {
	t.Helper()
	p, err := buildToolPolicy(cfg)
	if err != nil {
		t.Fatalf("buildToolPolicy: %v", err)
	}
	return p
}

// Flag true with a compiled policy: filtering runs, and the line says so.
func TestInstallToolListFiltering_PolicyAndFlagOn_Enabled(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig()
	on := true
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &on

	installToolListFiltering(newTestServer(), cfg, policyFrom(t, cfg), "groups")

	assertPosture(t, findPostureLine(t, buf), postureEnabled, "INFO")
}

// Flag omitted: the shipped default. Filtering is off and nothing is wrong, so
// the line is INFO and names the flag rather than blaming authorization.
func TestInstallToolListFiltering_FlagAbsent_DisabledAtInfo(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig() // FilterToolsList left nil

	installToolListFiltering(newTestServer(), cfg, policyFrom(t, cfg), "groups")

	rec := findPostureLine(t, buf)
	assertPosture(t, rec, postureDisabled, "INFO")
	assertReason(t, rec, postureReasonFlagNotSet)
}

// Flag explicitly false behaves exactly as absent. Both are off; the *bool
// distinction exists for a future default flip, not for today's behaviour.
func TestInstallToolListFiltering_FlagExplicitlyFalse_DisabledAtInfo(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig()
	off := false
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &off

	installToolListFiltering(newTestServer(), cfg, policyFrom(t, cfg), "groups")

	assertPosture(t, findPostureLine(t, buf), postureDisabled, "INFO")
}

// The flag is on but tool authorization is off, so there is no policy to filter
// against. WARN naming the reason — the operator asked for filtering and is not
// getting it — but startup continues: the state is inert, not incorrect, and it
// is what flipping the master switch during an incident produces.
func TestInstallToolListFiltering_FlagOnAuthzOff_WarnsAndContinues(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig()
	authzOff := false
	cfg.MCPClientAuth.ToolAuthorization.Enabled = &authzOff
	on := true
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &on

	policy := policyFrom(t, cfg)
	if policy != nil {
		t.Fatalf("precondition: expected nil policy with authorization disabled, got %v", policy)
	}

	// Reaching the assertions at all is half the test: installation must not
	// panic or exit when the flag is set with no policy behind it.
	installToolListFiltering(newTestServer(), cfg, policy, "groups")

	rec := findPostureLine(t, buf)
	assertPosture(t, rec, postureDisabled, "WARN")
	assertReason(t, rec, postureReasonAuthzOff)
}

// Authorization off and the flag also unset — the default posture for every
// deployment that never opts in. INFO, and it must not blame authorization,
// because the operator never asked for filtering.
func TestInstallToolListFiltering_AuthzOffFlagAbsent_DisabledAtInfo(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig()
	authzOff := false
	cfg.MCPClientAuth.ToolAuthorization.Enabled = &authzOff

	installToolListFiltering(newTestServer(), cfg, policyFrom(t, cfg), "groups")

	rec := findPostureLine(t, buf)
	assertPosture(t, rec, postureDisabled, "INFO")
	// Must not blame authorization: the operator never asked for filtering.
	assertReason(t, rec, postureReasonFlagNotSet)
}

// Non-oauth mode omits the whole tool_authorization block, so the flag is
// unreadable rather than false. Must not panic on the nil block.
func TestInstallToolListFiltering_NoToolAuthorizationBlock_DisabledAtInfo(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeDisabled},
	}

	installToolListFiltering(newTestServer(), cfg, policyFrom(t, cfg), "groups")

	assertPosture(t, findPostureLine(t, buf), postureDisabled, "INFO")
}
