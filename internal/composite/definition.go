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

// Package composite provides a YAML-driven composite tool engine for the Solace
// Broker MCP Server. It loads multi-step tool definitions from embedded YAML,
// executes steps against a broker via the sempv2.Client interface, resolves Go
// template expressions in step arguments, and combines results using configurable
// strategies (collect, postProcess).
package composite

// CompositeToolsFile is the top-level structure of the embedded YAML file.
type CompositeToolsFile struct {
	Tools []CompositeTool `yaml:"tools"`
}

// CompositeTool defines a multi-step tool loaded from YAML. Each tool has input
// parameters, an ordered list of steps to execute against a broker, and a result
// strategy that controls how step outputs are combined into the final response.
type CompositeTool struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Parameters  []ParameterDef  `yaml:"parameters"`
	Steps       []Step          `yaml:"steps"`
	Result      ResultStrategy  `yaml:"result"`
	Annotations ToolAnnotations `yaml:"annotations"`
}

// ParameterDef defines an input parameter for a composite tool. These are
// declared in YAML and used to generate the MCP tool's JSON Schema and to
// populate the template context for step argument resolution.
type ParameterDef struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Description string `yaml:"description"`
	IsPathParam bool   `yaml:"-"`
}

// Step defines a single operation in a composite tool. Each step references a
// SEMPv2 operation by its prefixed ID (e.g., "monitor/getMsgVpnQueue") and
// provides arguments as Go text/template expressions that are resolved against
// the input parameters and prior step results.
//
// Fan-out. When ForEach is set the step iterates a prior step's data[] rows
// concurrently instead of running once. Per iteration, the row is bound to
// .Item in the template context so Args templates can reference row fields.
// The result is a map keyed by row[ForEachKey] under a top-level "byKey" key —
// see fetchFanOut in executor.go for the exact shape. Fan-out has its own
// bounded concurrency (Concurrency, default fanOutDefaultConcurrency) and is
// not combinable with Parallel (validated at load time).
type Step struct {
	ID          string            `yaml:"id"`
	Operation   string            `yaml:"operation"`   // prefixed operationId (e.g., "monitor/getMsgVpnQueue")
	Args        map[string]string `yaml:"args"`        // values are Go text/template expressions
	Select      []string          `yaml:"select"`      // SEMP select fields; joined with ", " into args["select"] at execute time
	Parallel    bool              `yaml:"parallel"`    // group with adjacent parallel:true steps
	FollowPages bool              `yaml:"followPages"` // follow SEMP nextPageUri links and aggregate all pages
	ForEach     string            `yaml:"forEach"`     // step ID of a prior step whose data[] rows this step iterates
	ForEachIf   string            `yaml:"forEachIf"`   // optional predicate template; iteration skipped when it resolves to a false bool
	ForEachKey  string            `yaml:"forEachKey"`  // parent-row field whose value keys this step's result map (required with ForEach)
	Concurrency int               `yaml:"concurrency"` // max in-flight per-row calls in fan-out; 0 means use the framework default
}

// ResultStrategy defines how step results are combined into the tool's final
// output. "collect" returns all step results keyed by step ID. "postProcess"
// runs a registered Go postprocessor (see internal/composite/postprocess) on
// the collected step results and merges its output under a top-level "summary"
// key alongside the raw step results.
type ResultStrategy struct {
	Strategy    string `yaml:"strategy"`    // "collect" or "postProcess"
	PostProcess string `yaml:"postProcess"` // registry name of the handler when strategy="postProcess"
}

// ToolAnnotations holds behavior hints for a composite tool, matching the MCP
// spec 2025-06-18 ToolAnnotations. These are declared in YAML and mapped to
// the MCP SDK's ToolAnnotations during tool registration. Pointer fields
// distinguish "omitted" (nil → SDK default applies) from "explicitly false".
type ToolAnnotations struct {
	ReadOnly    *bool `yaml:"readOnly"`
	Destructive *bool `yaml:"destructive"`
	Idempotent  *bool `yaml:"idempotent"`
	OpenWorld   *bool `yaml:"openWorld"`
}
