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
	"bytes"
	"fmt"
	"io/fs"

	"gopkg.in/yaml.v3"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parsing composite tool file %q: %w", filename, err)
	}

	for i := range file.Tools {
		if err := validateTool(&file.Tools[i]); err != nil {
			return nil, fmt.Errorf("validation failed for tool %q: %w", file.Tools[i].Name, err)
		}
	}

	return file.Tools, nil
}

// validateTool checks the structural correctness of a composite tool definition:
// required fields, unique step IDs, mutually exclusive `select` forms, reserved
// step IDs, and a supported result strategy. This is the first of two
// validation passes — the second, ValidatePostProcess, runs after the
// postprocess registry is populated and cross-checks each handler's
// RequiredFields against the step `select:` clauses.
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

		// The structured `select:` and a templated `args.select` would race at
		// execute time (applySelect in executor.go overwrites args["select"]).
		// Reject the ambiguity at load time.
		if len(step.Select) > 0 {
			if _, dup := step.Args["select"]; dup {
				return fmt.Errorf("step %s: cannot set both args.select and select", step.ID)
			}
		}
	}

	if tool.Result.Strategy == "" {
		return fmt.Errorf("result strategy is required; supported values: collect, postProcess")
	}

	switch tool.Result.Strategy {
	case "collect":
		if tool.Result.PostProcess != "" {
			return fmt.Errorf("postProcess must be empty when strategy is %q", tool.Result.Strategy)
		}
	case "postProcess":
		if tool.Result.PostProcess == "" {
			return fmt.Errorf("postProcess is required when strategy is %q", tool.Result.Strategy)
		}
		// "summary" is reserved at the top level of a postProcess result for the
		// handler's output (see ApplyResultStrategy). A step named "summary"
		// would silently shadow it.
		if stepIDs["summary"] {
			return fmt.Errorf(`step ID "summary" is reserved when strategy is "postProcess"`)
		}
	default:
		return fmt.Errorf("result strategy %q is not supported; supported values: collect, postProcess", tool.Result.Strategy)
	}

	return nil
}

// ValidatePostProcess cross-checks every postProcess tool's postprocessor
// against the union of its steps' `select:` clauses. It runs as a second pass
// after LoadTools because handlers register from init() and the postprocess
// registry must be populated before this check is meaningful. Errors use the
// uniform template defined by postprocess.ValidateTool so a missing field
// surfaces the same message regardless of which tool tripped it.
//
// TODO multi-step: RequiredFields is checked against the union across all
// steps, so a postprocessor reading field X from step A passes validation
// when only step B selects X. Today every postProcess tool has one step
// (list-queues), so the union is precise. When a multi-step postProcess
// tool lands, change Handler.RequiredFields to map[stepID][]string and
// validate per step.
func ValidatePostProcess(tools []CompositeTool) error {
	for _, t := range tools {
		if t.Result.Strategy != "postProcess" {
			continue
		}
		var selectFields []string
		for _, s := range t.Steps {
			selectFields = append(selectFields, s.Select...)
		}
		if err := postprocess.ValidateTool(t.Name, t.Result.PostProcess, selectFields); err != nil {
			return err
		}
	}
	return nil
}
