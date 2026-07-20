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
	"slices"

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
	stepSelects := make(map[string][]string, len(tool.Steps))
	for _, step := range tool.Steps {
		if step.ID == "" {
			return fmt.Errorf("step ID is required")
		}

		if stepIDs[step.ID] {
			return fmt.Errorf("duplicate step ID: %s", step.ID)
		}
		stepIDs[step.ID] = true
		stepSelects[step.ID] = step.Select

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

		if err := validateFanOut(step, stepIDs, stepSelects); err != nil {
			return err
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
		// ValidatePostProcess cross-checks a handler's RequiredFields against the
		// structured `select:` only — it has no view into a templated args.select.
		// Allowing args.select here would pass the loader, then trip the postprocess
		// cross-check with a "reads X but it is not in select" message that points
		// at the field instead of the real cause. Reject up front so the requirement
		// is visible at the source.
		for _, step := range tool.Steps {
			if _, has := step.Args["select"]; has {
				return fmt.Errorf(`step %s: args.select is not allowed when strategy is "postProcess"; declare fields under top-level select`, step.ID)
			}
		}
	default:
		return fmt.Errorf("result strategy %q is not supported; supported values: collect, postProcess", tool.Result.Strategy)
	}

	return nil
}

// validateFanOut checks the fan-out-specific fields on a step. Called from
// validateTool during the step-loop walk so stepIDs and stepSelects are
// populated only for steps seen SO FAR — that is what enforces the
// forward-reference ban (a fan-out step cannot name a step declared later).
//
// The rules mirror the plan:
//   - ForEach must name a step already seen (no forward refs).
//   - ForEach + Parallel is rejected (fan-out has its own concurrency).
//   - ForEach requires a non-empty ForEachKey.
//   - When the parent step declares a non-empty select, ForEachKey must be in it,
//     so the key is guaranteed to reach the executor on parent rows.
//   - Concurrency is in [0, fanOutMaxConcurrency]; 0 means "use the framework default".
//   - Any fan-out field on a step without ForEach is a config smell and rejected.
func validateFanOut(step Step, priorStepIDs map[string]bool, priorStepSelects map[string][]string) error {
	if step.ForEach == "" {
		if step.ForEachIf != "" || step.ForEachKey != "" || step.Concurrency != 0 {
			return fmt.Errorf("step %s: forEachIf/forEachKey/concurrency require forEach to be set", step.ID)
		}
		return nil
	}
	if step.Parallel {
		return fmt.Errorf("step %s: forEach cannot be combined with parallel (fan-out has its own bounded concurrency)", step.ID)
	}
	if !priorStepIDs[step.ForEach] {
		return fmt.Errorf("step %s: forEach references step %q which is not declared before this step", step.ID, step.ForEach)
	}
	if step.ForEachKey == "" {
		return fmt.Errorf("step %s: forEach requires forEachKey", step.ID)
	}
	if parentSelect := priorStepSelects[step.ForEach]; len(parentSelect) > 0 && !slices.Contains(parentSelect, step.ForEachKey) {
		return fmt.Errorf("step %s: forEachKey %q is not in parent step %q's select — it will be missing on iteration rows", step.ID, step.ForEachKey, step.ForEach)
	}
	if step.Concurrency < 0 || step.Concurrency > fanOutMaxConcurrency {
		return fmt.Errorf("step %s: concurrency %d out of range [0, %d]", step.ID, step.Concurrency, fanOutMaxConcurrency)
	}
	return nil
}

// ValidatePostProcess cross-checks every postProcess tool's postprocessor
// against the tool's step IDs (Handler.RequiredSteps) and each step's own
// `select:` clause. It runs as a second pass after LoadTools because handlers
// register from init() and the postprocess registry must be populated before
// this check is meaningful. Errors use the uniform template defined by
// postprocess.ValidateTool so a missing step or field surfaces the same
// message regardless of which tool tripped it.
//
// Handlers declaring RequiredFieldsPerStep are validated per step; handlers
// still on the flat RequiredFields form are validated against the union of
// all steps' selects (same behaviour as before the multi-step extension).
func ValidatePostProcess(tools []CompositeTool) error {
	for _, t := range tools {
		if t.Result.Strategy != "postProcess" {
			continue
		}
		stepIDs := make([]string, 0, len(t.Steps))
		selectFieldsByStep := make(map[string][]string, len(t.Steps))
		for _, s := range t.Steps {
			stepIDs = append(stepIDs, s.ID)
			selectFieldsByStep[s.ID] = s.Select
		}
		if err := postprocess.ValidateTool(t.Name, t.Result.PostProcess, stepIDs, selectFieldsByStep); err != nil {
			return err
		}
	}
	return nil
}
