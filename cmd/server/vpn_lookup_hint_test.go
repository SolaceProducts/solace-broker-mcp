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
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Fragments of the three hint variants internal/tools appends. The exact
// sentences are pinned by internal/tools' unit tests; this test only needs to
// tell the variants apart in what the agent actually receives.
const (
	fanOutFragment    = "once for each VPN"
	confirmFragment   = "confirm the target VPN with the user"
	createVPNFragment = "which names are already taken"
)

// TestEveryVPNScopedToolPointsAtListVPNs pins SOL-158044. Claude Code defers
// MCP tool schemas behind ToolSearch, so an agent often sees one tool on its
// own; a tool that requires msgVpnName must therefore say, in what the agent
// actually receives, how to find a VPN name. This drives the full production
// registration pipeline over a real tools/list round-trip (registeredServer,
// listAllTools), so a new tool — composite or native — that skips the hint,
// or gets the wrong variant, fails here.
func TestEveryVPNScopedToolPointsAtListVPNs(t *testing.T) {
	allTools := listAllTools(t, registeredServer(t, true))

	names := make([]string, 0, len(allTools))
	for _, tool := range allTools {
		names = append(names, tool.Name)
	}
	// Every hint sends the agent to list-vpns; if it is renamed or removed
	// they all point at nothing.
	if !slices.Contains(names, "list-vpns") {
		t.Fatal("list-vpns is not registered, but every msgVpnName hint points at it")
	}
	// create-message-vpn is special-cased by name in internal/tools; if it is
	// renamed, the special case silently stops matching.
	if !slices.Contains(names, "create-message-vpn") {
		t.Fatal("create-message-vpn is not registered; update the special case in injectVPNLookupHint")
	}

	checked := 0
	for _, tool := range allTools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: marshal input schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: unmarshal input schema: %v", tool.Name, err)
		}
		if !slices.Contains(schema.Required, "msgVpnName") {
			continue
		}
		checked++

		desc := schema.Properties["msgVpnName"].Description
		if !strings.Contains(desc, "list-vpns") {
			t.Errorf("%s: msgVpnName description does not mention list-vpns: %q", tool.Name, desc)
			continue
		}

		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		// broker and msgVpnName are the only required parameters of a tool
		// that is scoped by VPN alone (a list or VPN-summary tool).
		vpnOnly := len(schema.Required) == 2 && slices.Contains(schema.Required, "broker")
		switch {
		case tool.Name == "create-message-vpn":
			if !strings.Contains(desc, createVPNFragment) {
				t.Errorf("%s: want the new-VPN hint, got %q", tool.Name, desc)
			}
		case !readOnly:
			if !strings.Contains(desc, confirmFragment) || strings.Contains(desc, fanOutFragment) {
				t.Errorf("%s: write tool must get the confirm-with-user hint and no fan-out: %q", tool.Name, desc)
			}
		case strings.Contains(desc, confirmFragment):
			// A read-only tool told to have the user confirm the VPN would
			// reintroduce exactly the question this hint exists to avoid.
			t.Errorf("%s: read-only tool got the write tool's confirm-with-user hint: %q", tool.Name, desc)
		case vpnOnly:
			if !strings.Contains(desc, fanOutFragment) {
				t.Errorf("%s: read-only VPN-scoped tool is missing the fan-out hint: %q", tool.Name, desc)
			}
		default:
			if strings.Contains(desc, fanOutFragment) {
				t.Errorf("%s: tool also needs an object identifier, so must not be told to fan out: %q", tool.Name, desc)
			}
		}
	}

	// 53 composite tools plus native get-queue-metrics at authoring time. A
	// floor, not an exact count, so adding a tool never needs this edited —
	// but a regression that stops listing VPN-scoped tools (or stops parsing
	// their schemas) cannot pass vacuously.
	if checked < 54 {
		t.Errorf("checked %d tools requiring msgVpnName, want at least 54", checked)
	}
}
