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
// strategies (collect, merge, unwrap).
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
}

// Step defines a single operation in a composite tool. Each step references a
// SEMPv2 operation by its prefixed ID (e.g., "monitor/getMsgVpnQueue") and
// provides arguments as Go text/template expressions that are resolved against
// the input parameters and prior step results.
type Step struct {
	ID          string            `yaml:"id"`
	Operation   string            `yaml:"operation"`   // prefixed operationId (e.g., "monitor/getMsgVpnQueue")
	Args        map[string]string `yaml:"args"`        // values are Go text/template expressions
	Parallel    bool              `yaml:"parallel"`    // group with adjacent parallel:true steps
	FollowPages bool              `yaml:"followPages"` // follow SEMP nextPageUri links and aggregate all pages
}

// ResultStrategy defines how step results are combined into the tool's final
// output. Currently only "collect" is supported, which returns all step results
// keyed by step ID. Additional strategies (merge, unwrap) are deferred pending
// design discussion around SEMP response envelope overlap and per-step data
// transformation needs.
type ResultStrategy struct {
	Strategy string `yaml:"strategy"` // only "collect" is currently supported
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
