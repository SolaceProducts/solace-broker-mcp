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

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/handlers" // register handlers via init()
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
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
		const wantToolCount = 30
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

	t.Run("operations", func(t *testing.T) {
		operations, err := sempv2.ParseSpecs(specs.FS)
		if err != nil {
			t.Fatalf("ParseSpecs: %v", err)
		}
		for _, tool := range tools {
			for _, step := range tool.Steps {
				if _, exists := operations[step.Operation]; !exists {
					t.Errorf("tool %q step %q: operation %q not found in spec", tool.Name, step.ID, step.Operation)
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
