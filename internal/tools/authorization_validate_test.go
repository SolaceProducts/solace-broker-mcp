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

package tools

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// These tests cover the startup validator that catches admin typos in
// tool_authorization YAML. Assertions target the startup log (error rows,
// WARN lines) and the aggregate return contract main.go branches on.

// validateTestManager builds a ToolManager with stubs for the given names.
func validateTestManager(t *testing.T, toolNames ...string) *ToolManager {
	t.Helper()
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	for _, name := range toolNames {
		mgr.Register(newStubHandler(name))
	}
	return mgr
}

// warnLinesMentioning returns every WARN JSON slog line containing substr.
func warnLinesMentioning(t *testing.T, buf *bytes.Buffer, substr string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if entry["level"] != "WARN" {
			continue
		}
		if strings.Contains(raw, substr) {
			out = append(out, entry)
		}
	}
	return out
}

// errorRowCount counts newline-joined rows in an errors.Join'd error string.
func errorRowCount(err error) int {
	if err == nil {
		return 0
	}
	return strings.Count(err.Error(), "\n") + 1
}

// All-known config → nil error and zero log output (silent no-op).
func TestValidatePolicyToolNames_AllKnown_ReturnsNil(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	mgr := validateTestManager(t, "get-broker-status", "list-queues")
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Ops":        {"get-broker-status", "list-queues"},
			"Monitoring": {"get-broker-status"},
		},
	}

	if err := ValidatePolicyToolNames(cfg, mgr); err != nil {
		t.Errorf("expected nil error for all-known config, got: %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("expected zero log output on all-known config; got: %s", buf.String())
	}
}

// One unknown tool → one error row naming both the tool and the group.
func TestValidatePolicyToolNames_OneUnknown_ReportsToolAndGroup(t *testing.T) {
	mgr := validateTestManager(t, "list-queues")
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Ops": {"list-queues", "not-a-real-tool"},
		},
	}

	err := ValidatePolicyToolNames(cfg, mgr)
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	if got := errorRowCount(err); got != 1 {
		t.Errorf("expected 1 error row, got %d: %s", got, err.Error())
	}
	msg := err.Error()
	if !strings.Contains(msg, "not-a-real-tool") {
		t.Errorf("error missing unknown tool name; got: %s", msg)
	}
	if !strings.Contains(msg, "Ops") {
		t.Errorf("error missing referencing group name; got: %s", msg)
	}
}

// Same typo across N groups → one deduped row; groups alphabetized.
func TestValidatePolicyToolNames_SameUnknownAcrossGroups_DedupedToOneRow(t *testing.T) {
	mgr := validateTestManager(t, "list-queues")
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Zeta":  {"missing-tool"},
			"Alpha": {"missing-tool"},
			"Mu":    {"missing-tool"},
		},
	}

	err := ValidatePolicyToolNames(cfg, mgr)
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	if got := errorRowCount(err); got != 1 {
		t.Errorf("expected 1 deduped row for one typo referenced 3 times, got %d rows: %s", got, err.Error())
	}
	msg := err.Error()
	alphaIdx := strings.Index(msg, "Alpha")
	muIdx := strings.Index(msg, "Mu")
	zetaIdx := strings.Index(msg, "Zeta")
	if alphaIdx < 0 || muIdx < 0 || zetaIdx < 0 {
		t.Fatalf("expected all three group names in error message; got: %s", msg)
	}
	if alphaIdx >= muIdx || muIdx >= zetaIdx {
		t.Errorf("expected referencing groups alphabetized (Alpha < Mu < Zeta); got: %s", msg)
	}
}

// Multiple unknown tools → rows alphabetized (deterministic across restarts).
func TestValidatePolicyToolNames_MultipleUnknowns_AlphabeticalOrder(t *testing.T) {
	mgr := validateTestManager(t)
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"g1": {"zebra-tool", "apple-tool", "mango-tool"},
		},
	}

	err := ValidatePolicyToolNames(cfg, mgr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := errorRowCount(err); got != 3 {
		t.Errorf("expected 3 rows, got %d: %s", got, err.Error())
	}
	msg := err.Error()
	appleIdx := strings.Index(msg, "apple-tool")
	mangoIdx := strings.Index(msg, "mango-tool")
	zebraIdx := strings.Index(msg, "zebra-tool")
	if appleIdx >= mangoIdx || mangoIdx >= zebraIdx {
		t.Errorf("expected unknown-tool rows alphabetized (apple < mango < zebra); got: %s", msg)
	}
}

// list-brokers grant → WARN, not error; startup proceeds.
func TestValidatePolicyToolNames_ListBrokersGrant_WarnsNotError(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	mgr := validateTestManager(t, "get-broker-status")
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Ops": {"get-broker-status", "list-brokers"},
		},
	}

	if err := ValidatePolicyToolNames(cfg, mgr); err != nil {
		t.Errorf("list-brokers grant must not surface as an error; got: %v", err)
	}

	warns := warnLinesMentioning(t, buf, "list-brokers")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 WARN line about list-brokers, got %d: %s", len(warns), buf.String())
	}
	// WARN must name the referencing group so the admin can find it.
	if !strings.Contains(buf.String(), "Ops") {
		t.Errorf("WARN missing referencing group name Ops: %s", buf.String())
	}
}

// Typo + list-brokers grant on one config → both surfaces fire independently.
func TestValidatePolicyToolNames_UnknownsAndListBrokers_BothSurface(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	mgr := validateTestManager(t, "get-broker-status")
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Ops":         {"get-broker-status", "delet-queue", "list-brokers"},
			"Contractors": {"list-brokers"},
		},
	}

	err := ValidatePolicyToolNames(cfg, mgr)
	if err == nil {
		t.Fatal("expected error for typo, got nil")
	}
	if !strings.Contains(err.Error(), "delet-queue") {
		t.Errorf("error missing typo name; got: %s", err.Error())
	}

	warns := warnLinesMentioning(t, buf, "list-brokers")
	if len(warns) != 1 {
		t.Fatalf("expected 1 WARN even with typo present, got %d: %s", len(warns), buf.String())
	}
}

// Case-mismatched "List-Brokers" is unknown (not exempt) — no silent case-folding.
func TestValidatePolicyToolNames_CaseMismatchIsUnknown(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	mgr := validateTestManager(t)
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			"Ops": {"List-Brokers"},
		},
	}

	err := ValidatePolicyToolNames(cfg, mgr)
	if err == nil {
		t.Fatal("expected error for case-mismatched list-brokers, got nil (silent case-folding would be a security drift)")
	}
	if !strings.Contains(err.Error(), "List-Brokers") {
		t.Errorf("error missing case-mismatched name; got: %s", err.Error())
	}
	// And no list-brokers WARN — case-folded match would have emitted one.
	if warns := warnLinesMentioning(t, buf, "list-brokers is exempt"); len(warns) > 0 {
		t.Errorf("case-mismatched name should not trigger list-brokers WARN; got: %s", buf.String())
	}
}

// Nil or empty AccessLevelGroups → nil, no log output (fail-safe).
func TestValidatePolicyToolNames_EmptyAccessLevelGroups_ReturnsNil(t *testing.T) {
	mgr := validateTestManager(t, "get-broker-status")
	cases := []struct {
		name string
		alg  map[string][]string
	}{
		{"nil map", nil},
		{"empty map", map[string][]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, cleanup := captureSlog(t)
			defer cleanup()

			cfg := config.ToolAuthorizationConfig{AccessLevelGroups: tc.alg}
			if err := ValidatePolicyToolNames(cfg, mgr); err != nil {
				t.Errorf("expected nil for %s, got: %v", tc.name, err)
			}
			if buf.Len() > 0 {
				t.Errorf("expected no log output for %s, got: %s", tc.name, buf.String())
			}
		})
	}
}
