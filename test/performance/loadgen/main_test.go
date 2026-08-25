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

package main

import (
	"reflect"
	"strings"
	"testing"
)

// fullArgs is a value for every argument name any recipe references. Tests
// that care about a missing value override one entry.
func fullArgs() map[string]string {
	return map[string]string{argVPN: "vpn_2", argRDP: "rdp_1"}
}

// TestBuildCallSpecsArguments pins the exact argument map each tool is called
// with. These are the values that reach the broker, so a wrong or missing one
// would surface only as an IsError buried in the run's error rate.
func TestBuildCallSpecsArguments(t *testing.T) {
	tests := []struct {
		tool string
		want map[string]any
	}{
		{
			tool: "get-broker-status",
			want: map[string]any{"broker": "broker-01"},
		},
		{
			tool: "list-queues",
			want: map[string]any{"broker": "broker-01", "msgVpnName": "vpn_2"},
		},
		{
			tool: "list-rdps",
			want: map[string]any{"broker": "broker-01", "msgVpnName": "vpn_2"},
		},
		{
			tool: "get-rdp-status",
			want: map[string]any{
				"broker":                "broker-01",
				"msgVpnName":            "vpn_2",
				"restDeliveryPointName": "rdp_1",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			specs := buildCallSpecs([]string{tc.tool}, "broker-01", fullArgs())
			if len(specs) != 1 {
				t.Fatalf("buildCallSpecs returned %d specs, want 1", len(specs))
			}
			if specs[0].tool != tc.tool {
				t.Errorf("tool = %q, want %q", specs[0].tool, tc.tool)
			}
			if !reflect.DeepEqual(specs[0].args, tc.want) {
				t.Errorf("args = %v, want %v", specs[0].args, tc.want)
			}
		})
	}
}

// TestBuildCallSpecsPreservesRotationOrder guards the round-robin: clientLoop
// indexes specs modulo their length, so the order must match -tools.
func TestBuildCallSpecsPreservesRotationOrder(t *testing.T) {
	tools := []string{"list-rdps", "get-rdp-status", "list-queues"}
	specs := buildCallSpecs(tools, "broker-07", fullArgs())

	got := make([]string, 0, len(specs))
	for _, s := range specs {
		got = append(got, s.tool)
	}
	if !reflect.DeepEqual(got, tools) {
		t.Errorf("spec order = %v, want %v", got, tools)
	}
}

// TestBuildCallSpecsIsolatesBrokers checks each client's specs carry that
// client's own broker — a shared map would pin every client to one broker and
// silently invalidate the fan-out the run is measuring.
func TestBuildCallSpecsIsolatesBrokers(t *testing.T) {
	first := buildCallSpecs([]string{"list-rdps"}, "broker-01", fullArgs())
	second := buildCallSpecs([]string{"list-rdps"}, "broker-02", fullArgs())

	if first[0].args["broker"] != "broker-01" {
		t.Errorf("first broker = %v, want broker-01", first[0].args["broker"])
	}
	if second[0].args["broker"] != "broker-02" {
		t.Errorf("second broker = %v, want broker-02", second[0].args["broker"])
	}
}

// TestValidateToolsRejectsUnknownTool is the acceptance case for "loadgen
// refuses to start when -tools names a tool it has no argument recipe for,
// with a message naming the tool".
func TestValidateToolsRejectsUnknownTool(t *testing.T) {
	err := validateTools([]string{"list-rdps", "list-widgets"}, fullArgs())
	if err == nil {
		t.Fatal("validateTools accepted an unknown tool; the run must not start")
	}
	if !strings.Contains(err.Error(), "list-widgets") {
		t.Errorf("error does not name the offending tool: %v", err)
	}
	// The message has to be actionable — an operator who typo'd a tool name
	// needs to see what the valid names are.
	for _, known := range []string{"get-broker-status", "get-rdp-status", "list-queues", "list-rdps"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error does not list known tool %q: %v", known, err)
		}
	}
}

// TestValidateToolsRejectsMissingArgValue covers the other half of the gap:
// a known tool whose required argument was never supplied. Calling
// get-rdp-status with an empty restDeliveryPointName returns IsError, which
// is indistinguishable from a broker problem in the reported error rate.
func TestValidateToolsRejectsMissingArgValue(t *testing.T) {
	args := fullArgs()
	args[argRDP] = ""

	err := validateTools([]string{"get-rdp-status"}, args)
	if err == nil {
		t.Fatal("validateTools accepted get-rdp-status with no -rdp value")
	}
	if !strings.Contains(err.Error(), argRDP) {
		t.Errorf("error does not name the missing argument: %v", err)
	}

	// A tool that doesn't need the empty argument is unaffected.
	if err := validateTools([]string{"list-rdps"}, args); err != nil {
		t.Errorf("list-rdps rejected for an argument it does not use: %v", err)
	}
}

// TestValidateToolsAcceptsEveryRecipe keeps the recipe table and the default
// -tools value honest: everything declared must be callable with the flags
// loadgen offers.
func TestValidateToolsAcceptsEveryRecipe(t *testing.T) {
	all := make([]string, 0, len(toolRecipes))
	for name := range toolRecipes {
		all = append(all, name)
	}
	if err := validateTools(all, fullArgs()); err != nil {
		t.Fatalf("validateTools rejected a declared recipe: %v", err)
	}
}
