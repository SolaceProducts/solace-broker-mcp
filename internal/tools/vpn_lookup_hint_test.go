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

package tools

import (
	"strings"
	"testing"
)

// vpnSchema returns a fresh input schema declaring msgVpnName with the given
// description, required or not, plus a queueName that is required only when
// withObjectID is set — the shape of a get-one-object tool such as
// get-queue-metrics, as opposed to a list tool scoped by VPN alone.
func vpnSchema(desc string, required bool, withObjectID bool) map[string]any {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string", "description": desc},
			"queueName":  map[string]any{"type": "string"},
		},
	}
	var req []string
	if required {
		req = append(req, "msgVpnName")
	}
	if withObjectID {
		req = append(req, "queueName")
	}
	if len(req) > 0 {
		schema["required"] = req
	}
	return schema
}

// advertisedVPNDescription runs m through toMCPTool — the path every tool
// takes to the agent — and returns the msgVpnName description it advertises.
func advertisedVPNDescription(t *testing.T, m Metadata) string {
	t.Helper()
	tool := toMCPTool(m, newRegTestPool(t))
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("InputSchema is %T, want map[string]any", tool.InputSchema)
	}
	prop, ok := schema["properties"].(map[string]any)["msgVpnName"].(map[string]any)
	if !ok {
		t.Fatal("advertised schema lost the msgVpnName property")
	}
	desc, _ := prop["description"].(string)
	return desc
}

// SOL-158044: a read-only tool tells the agent to look the VPN up and, for a
// broker-wide question, to fan out over every VPN.
func TestToMCPTool_VPNHint_ReadOnly(t *testing.T) {
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "list-slow-subscribers",
		InputSchema: vpnSchema("The Message VPN to search for slow subscribers", true, false),
		Annotations: Annotations{ReadOnly: true},
	})
	want := "The Message VPN to search for slow subscribers. " + vpnHintLookup + " " + vpnHintFanOut
	if desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
}

// A read-only tool that also needs an object identifier (a queue, a client)
// is told to look the VPN up but not to fan out: the object lives in one VPN,
// so calling the tool once per VPN would only produce not-found errors.
func TestToMCPTool_VPNHint_ReadOnlyWithObjectIDDoesNotFanOut(t *testing.T) {
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "get-queue-metrics",
		InputSchema: vpnSchema("The Message VPN containing the queue", true, true),
		Annotations: Annotations{ReadOnly: true},
	})
	want := "The Message VPN containing the queue. " + vpnHintLookup
	if desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
}

// SOL-158044: a write tool points at list-vpns but must not invite the agent
// to run it across every VPN — the read-only fan-out wording must not leak in.
func TestToMCPTool_VPNHint_WriteToolDoesNotInviteFanOut(t *testing.T) {
	destructive := true
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "delete-queue",
		InputSchema: vpnSchema("The Message VPN containing the queue", true, true),
		Annotations: Annotations{Destructive: &destructive},
	})
	want := "The Message VPN containing the queue. " + vpnHintWrite
	if desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
	if strings.Contains(desc, vpnHintFanOut) {
		t.Errorf("write-tool description invites a fan-out across VPNs: %q", desc)
	}
}

// SOL-158044: create-message-vpn names a VPN that does not exist yet, so
// "call list-vpns to find it" would be wrong; it gets its own sentence. Its
// base description already says the VPN is new, so the hint adds only the
// list-vpns pointer — spelled out literally here so a hint that repeats the
// description fails this test.
func TestToMCPTool_VPNHint_CreateMessageVPN(t *testing.T) {
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "create-message-vpn",
		InputSchema: vpnSchema("The name of the Message VPN to create", true, false),
	})
	want := "The name of the Message VPN to create. Call list-vpns to see which names are already taken."
	if desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
}

// A description that already ends in a full stop is not given a second one.
func TestToMCPTool_VPNHint_ExistingFullStop(t *testing.T) {
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "clear-queue-stats",
		InputSchema: vpnSchema("The Message VPN containing the queue.", true, true),
	})
	want := "The Message VPN containing the queue. " + vpnHintWrite
	if desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
}

// An optional msgVpnName is left alone: the hint is about a value the agent
// cannot proceed without.
func TestToMCPTool_VPNHint_OptionalUntouched(t *testing.T) {
	desc := advertisedVPNDescription(t, Metadata{
		Name:        "some-tool",
		InputSchema: vpnSchema("Optional VPN filter", false, false),
		Annotations: Annotations{ReadOnly: true},
	})
	if desc != "Optional VPN filter" {
		t.Errorf("optional msgVpnName description changed to %q", desc)
	}
}

// The handler's own schema is never modified: Metadata() may hand out a
// shared map, and ToolManager validates calls against it.
func TestToMCPTool_VPNHint_DoesNotMutateHandlerSchema(t *testing.T) {
	schema := vpnSchema("The Message VPN containing the queue", true, true)
	_ = advertisedVPNDescription(t, Metadata{
		Name:        "get-queue-metrics",
		InputSchema: schema,
		Annotations: Annotations{ReadOnly: true},
	})
	got := schema["properties"].(map[string]any)["msgVpnName"].(map[string]any)["description"]
	if got != "The Message VPN containing the queue" {
		t.Errorf("handler schema was mutated: description = %q", got)
	}
}
