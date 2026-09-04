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

package composite

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess/handlers" // register handlers via init()
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// kebabCaseRE matches valid kebab-case: lowercase letters/digits separated by single hyphens.
var kebabCaseRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func findTool(tools []CompositeTool, name string) *CompositeTool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func findStep(tool *CompositeTool, id string) *Step {
	for i := range tool.Steps {
		if tool.Steps[i].ID == id {
			return &tool.Steps[i]
		}
	}
	return nil
}

// createOrUpdateWriteTools names every create/update write tool — the ones
// whose operation echoes the resource back as response data, so both
// BodyFields and ResponseFields (and the generated strict output schema) are
// expected to resolve. Deliberately NOT keyed on "does the operation declare
// a body parameter": every action tool (disconnect-client, clear-*-stats,
// delete-queue-messages) also declares one, but resolves to a genuinely
// empty schema ({"properties": {}} in the spec) — a real, permanent
// difference from create/update's full-resource body, not a resolution
// failure. This is a distinct map from writeToolIdentifierFields in
// internal/tools/composite_handler.go — same underlying set of tools, kept
// as two lists because tools already imports composite, so the reverse
// import would cycle. writeToolIdentifierFields' own completeness is now
// enforced by internal/tools' TestWriteToolIdentifierFields_CoversEveryEligibleWriteTool;
// this list has no equivalent check and still needs a deliberate update
// alongside it, same convention as wantToolCount above.
var createOrUpdateWriteTools = map[string]bool{
	"create-message-vpn":        true,
	"update-message-vpn":        true,
	"create-queue":              true,
	"update-queue":              true,
	"create-queue-subscription": true,
	"create-topic-endpoint":     true,
	"update-topic-endpoint":     true,
	"create-rdp":                true,
	"update-rdp":                true,
}

func TestLoadTools_EmbeddedDefinitions(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools(definitions.FS): %v", err)
	}

	t.Run("count", func(t *testing.T) {
		// Exact count guards against silent drops and accidental additions — update deliberately.
		const wantToolCount = 37
		if len(tools) != wantToolCount {
			t.Errorf("tool count: got %d, want %d", len(tools), wantToolCount)
		}
	})

	t.Run("naming", func(t *testing.T) {
		seen := make(map[string]bool)
		for _, tool := range tools {
			if seen[tool.Name] {
				t.Errorf("duplicate tool name: %q", tool.Name)
			}
			seen[tool.Name] = true

			if !kebabCaseRE.MatchString(tool.Name) {
				t.Errorf("tool %q: invalid name (must be kebab-case)", tool.Name)
			}
		}
	})

	t.Run("postprocess", func(t *testing.T) {
		if err := ValidatePostProcess(tools); err != nil {
			t.Errorf("ValidatePostProcess: %v", err)
		}
	})

	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}

	t.Run("operations", func(t *testing.T) {
		for _, tool := range tools {
			for _, step := range tool.Steps {
				if _, exists := operations[step.Operation]; !exists {
					t.Errorf("tool %q step %q: operation %q not found in spec", tool.Name, step.ID, step.Operation)
				}
			}
		}
	})

	// Catches a wiring bug executor_*_test.go can't: those tests build
	// CompositeTool literals by hand and never load tools.yaml, so a real
	// path-param bug in the shipped YAML passes them untouched (confirmed by
	// injecting one). Checked here against the real embedded catalog instead.
	//
	// Fan-out steps still supply path params via step.Args today (list-vpns
	// templates msgVpnName from .Item). If a future fan-out step relies on
	// ForEachKey alone, update this check rather than deleting it.
	t.Run("path-params-wired", func(t *testing.T) {
		for _, tool := range tools {
			for _, step := range tool.Steps {
				op, ok := operations[step.Operation]
				if !ok {
					continue // already reported by the "operations" subtest above
				}
				for _, param := range sempv2.PathParamNames(op.Path) {
					if _, wired := step.Args[param]; !wired {
						t.Errorf("tool %q step %q (%s): path param %q not wired in args", tool.Name, step.ID, op.Path, param)
					}
				}
			}
		}
	})

	// SOL-152947: the CI breakage guard for write tools (config/action), the
	// same role path-params-wired plays for path wiring. constructRequestBody
	// already rejects an unrecognized body field at runtime via op.BodyFields
	// (internal/tools' output-schema generator similarly derives strictness
	// from op.ResponseFields) — but nothing proved those two extractions stay
	// resolvable against the real embedded catalog until now. A future spec
	// bump that breaks either resolution for a write tool (renamed/reshaped
	// body schema, envelope no longer exposing "data") degrades silently:
	// constructRequestBody falls back to skipping the unknown-field check
	// (BodyFields == nil), and the generated output schema falls back to
	// fully permissive (ResponseFields == nil) — both fail open, not closed.
	// This catches that at CI time instead.
	t.Run("write-tool-schema-resolves", func(t *testing.T) {
		for _, tool := range tools {
			if !createOrUpdateWriteTools[tool.Name] {
				continue
			}
			if len(tool.Steps) == 0 {
				t.Errorf("tool %q: no steps", tool.Name)
				continue
			}
			step := tool.Steps[0]
			op, ok := operations[step.Operation]
			if !ok {
				continue // already reported by the "operations" subtest above
			}

			if op.BodyFields == nil {
				t.Errorf("tool %q step %q (%s): BodyFields did not resolve — constructRequestBody's unknown-field check would silently stop working", tool.Name, step.ID, op.ID)
			}
			if op.ResponseFields == nil {
				t.Errorf("tool %q step %q (%s): ResponseFields did not resolve — the generated output schema would silently fall back to fully permissive", tool.Name, step.ID, op.ID)
			}

			schema := BuildStrictOutputSchema(tool, operations, nil)
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Errorf("tool %q: BuildStrictOutputSchema produced no top-level properties", tool.Name)
				continue
			}
			stepSchema, ok := props[step.ID].(map[string]any)
			if !ok {
				t.Errorf("tool %q step %q: no generated schema", tool.Name, step.ID)
				continue
			}
			if _, isStrict := stepSchema["additionalProperties"]; !isStrict {
				t.Errorf("tool %q step %q: generated schema fell back to the fully permissive shape ({\"type\":\"object\"} only) — the strict, spec-derived schema was expected here", tool.Name, step.ID)
			}
		}
	})

	// Spot-check get-replication-status — the tool that motivated this test (no e2e coverage).
	t.Run("spot/get-replication-status", func(t *testing.T) {
		tool := findTool(tools, "get-replication-status")
		if tool == nil {
			t.Fatal("tool not found")
			return
		}
		step := findStep(tool, "replication")
		if step == nil {
			t.Fatal("step 'replication' not found")
			return
		}
		if step.Operation != "monitor/getMsgVpn" {
			t.Errorf("operation: got %q, want %q", step.Operation, "monitor/getMsgVpn")
		}
		selectArg, ok := step.Args["select"]
		if !ok {
			t.Fatal("args.select not present")
			return
		}
		// Validate key replication fields are present in the comma-joined select string.
		for _, field := range []string{
			"replicationRole",
			"replicationSyncEligible",
			"replicationBridgeUp",
			"replicationTransactionMode",
		} {
			if !strings.Contains(selectArg, field) {
				t.Errorf("args.select missing %q", field)
			}
		}
	})

	// Spot-check a fan-out tool to catch YAML→struct decoding bugs in forEach/forEachKey fields.
	t.Run("spot/list-vpns", func(t *testing.T) {
		tool := findTool(tools, "list-vpns")
		if tool == nil {
			t.Fatal("tool not found")
			return
		}
		step := findStep(tool, "real-clients")
		if step == nil {
			t.Fatal("step 'real-clients' not found")
			return
		}
		if step.ForEach != "vpns" {
			t.Errorf("forEach: got %q, want %q", step.ForEach, "vpns")
		}
		if step.ForEachKey != "msgVpnName" {
			t.Errorf("forEachKey: got %q, want %q", step.ForEachKey, "msgVpnName")
		}
	})

	// Spot-check the create/delete asymmetry for queue subscriptions against
	// the shipped catalog (SOL-153868 PR #374, bczoma): subscriptionTopic is
	// deliberately omitted from create-queue-subscription's step args so
	// constructRequestBody spreads it into the POST body instead, but IS a
	// step arg on delete-queue-subscription, where it's a path segment.
	// loader_queue_subscription_test.go pins this against a hand-copied YAML
	// fixture, which stays green even if a future tools.yaml edit reintroduces
	// the arg — this checks the real embedded definition instead.
	t.Run("spot/create-queue-subscription", func(t *testing.T) {
		tool := findTool(tools, "create-queue-subscription")
		if tool == nil {
			t.Fatal("tool not found")
			return
		}
		if len(tool.Steps) == 0 {
			t.Fatal("no steps")
			return
		}
		if _, ok := tool.Steps[0].Args["subscriptionTopic"]; ok {
			t.Error("subscriptionTopic must not be a step arg on create — it belongs in the request body")
		}
	})
}

// TestLoadTools_ListVPNs_RetainsDiscoveryFields pins that list-vpns keeps
// replicationEnabled and dmrEnabled in its select list.
//
// These two sit in a validation blind spot. The framework's RequiredFields /
// RequiredFieldsPerStep checks (see ValidatePostProcess) only guard fields the
// postprocess handler consumes, and list_vpns.go references neither of these —
// it branches solely on enabled, state, and msgVpnName. So nothing else in the
// suite notices if they are dropped from the YAML.
//
// But they are the reason list-vpns is documented as the discovery step for
// "which VPNs are replicated": the values are consumed by the model reading the
// response, not by our Go code. Removing them would leave every existing test
// green while silently removing the tool's ability to answer that question, and
// the follow-up path (get-replication-status per VPN) has no other way to learn
// which VPNs are worth asking about.
//
// Both fields being genuinely independent was confirmed against a live broker
// during SOL-151996: a Message VPN with DR replication configured reported
// replicationEnabled=true AND dmrEnabled=true, while the reserved and default
// VPNs on the same broker reported false for both.
func TestLoadTools_ListVPNs_RetainsDiscoveryFields(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	tool := findTool(tools, "list-vpns")
	if tool == nil {
		t.Fatal("tool list-vpns not found in embedded definitions")
	}
	step := findStep(tool, "vpns")
	if step == nil {
		t.Fatal("step 'vpns' not found in list-vpns")
	}

	inSelect := make(map[string]bool, len(step.Select))
	for _, f := range step.Select {
		inSelect[f] = true
	}
	for _, field := range []string{"replicationEnabled", "dmrEnabled"} {
		if !inSelect[field] {
			t.Errorf("list-vpns step 'vpns' select is missing %q — the postprocess handler "+
				"does not read it, so no other test guards it, but the tool's documented "+
				"discovery role depends on it reaching the model; select = %v",
				field, step.Select)
		}
	}
}
