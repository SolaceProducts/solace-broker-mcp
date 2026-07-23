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
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/handlers" // register handlers via init()
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
)

func findTool(tools []CompositeTool, name string) *CompositeTool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
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
		// Guard against silent tool drops — update this floor when tools are intentionally removed.
		const minToolCount = 30
		if len(tools) < minToolCount {
			t.Errorf("tool count: got %d, want >= %d (silent drop?)", len(tools), minToolCount)
		}
	})

	t.Run("naming", func(t *testing.T) {
		seen := make(map[string]bool)
		for _, tool := range tools {
			if seen[tool.Name] {
				t.Errorf("duplicate tool name: %q", tool.Name)
			}
			seen[tool.Name] = true

			if tool.Name != strings.ToLower(tool.Name) {
				t.Errorf("tool %q: name contains uppercase (must be kebab-case)", tool.Name)
			}
			if strings.Contains(tool.Name, "_") {
				t.Errorf("tool %q: name contains underscore (must be kebab-case, not snake_case)", tool.Name)
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

	// Spot-check a fan-out tool to catch YAML→struct decoding bugs in forEach/forEachKey fields.
	t.Run("spot/list-vpns", func(t *testing.T) {
		tool := findTool(tools, "list-vpns")
		if tool == nil {
			t.Fatal("tool not found")
			return
		}
		if len(tool.Steps) < 2 {
			t.Fatalf("got %d steps, want >= 2 (vpns + fan-out probe)", len(tool.Steps))
		}
		var fanoutStep *Step
		for i := range tool.Steps {
			if tool.Steps[i].ForEach != "" {
				fanoutStep = &tool.Steps[i]
				break
			}
		}
		if fanoutStep == nil {
			t.Fatal("no fan-out step found")
			return
		}
		if fanoutStep.ForEach != "vpns" {
			t.Errorf("forEach: got %q, want %q", fanoutStep.ForEach, "vpns")
		}
		if fanoutStep.ForEachKey != "msgVpnName" {
			t.Errorf("forEachKey: got %q, want %q", fanoutStep.ForEachKey, "msgVpnName")
		}
	})
}
