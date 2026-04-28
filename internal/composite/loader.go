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
	"fmt"
	"io/fs"

	"gopkg.in/yaml.v3"
)

// LoadTools reads a YAML file from the given filesystem, parses it into
// composite tool definitions, and validates each tool's structure. It takes
// an fs.FS (rather than a file path) because tool definitions are embedded
// in the binary. This also makes testing easy — tests pass an in-memory FS.
func LoadTools(fsys fs.FS, filename string) ([]CompositeTool, error) {
	data, err := fs.ReadFile(fsys, filename)
	if err != nil {
		return nil, fmt.Errorf("reading composite tool file %q: %w", filename, err)
	}

	var file CompositeToolsFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing composite tool file %q: %w", filename, err)
	}

	for i := range file.Tools {
		if err := validateTool(&file.Tools[i]); err != nil {
			return nil, fmt.Errorf("validation failed for tool %q: %w", file.Tools[i].Name, err)
		}
	}

	return file.Tools, nil
}

// validateTool checks the structural correctness of a composite tool definition.
// It verifies required fields, unique step IDs, and valid result strategy
// configuration. It validates that a result strategy is specified and is a
// supported value.
func validateTool(tool *CompositeTool) error {
	if tool.Name == "" {
		return fmt.Errorf("tool name is required")
	}

	if tool.Description == "" {
		return fmt.Errorf("tool description is required")
	}

	if len(tool.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}

	stepIDs := make(map[string]bool, len(tool.Steps))
	for _, step := range tool.Steps {
		if step.ID == "" {
			return fmt.Errorf("step ID is required")
		}

		if stepIDs[step.ID] {
			return fmt.Errorf("duplicate step ID: %s", step.ID)
		}
		stepIDs[step.ID] = true

		if step.Operation == "" {
			return fmt.Errorf("step operation is required for step %s", step.ID)
		}
	}

	if tool.Result.Strategy == "" {
		return fmt.Errorf("result strategy is required; supported values: collect")
	}

	if tool.Result.Strategy != "collect" {
		return fmt.Errorf("result strategy %q is not supported; supported values: collect", tool.Result.Strategy)
	}

	return nil
}
