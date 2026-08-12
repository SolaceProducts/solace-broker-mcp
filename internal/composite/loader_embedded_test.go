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

func TestLoadTools_EmbeddedDefinitions(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools(definitions.FS): %v", err)
	}

	t.Run("count", func(t *testing.T) {
		// Exact count guards against silent drops and accidental additions — update deliberately.
		const wantToolCount = 34
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
