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

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
)

// stepSelectFields returns the SEMP select fields for a step, whether declared
// as a structured Select list (the list tools) or an inline args["select"]
// string (the get tools).
func stepSelectFields(step *Step) map[string]bool {
	out := map[string]bool{}
	for _, f := range step.Select {
		out[strings.TrimSpace(f)] = true
	}
	if s, ok := step.Args["select"]; ok {
		for _, f := range strings.Split(s, ",") {
			out[strings.TrimSpace(f)] = true
		}
	}
	return out
}

// TestLoadTools_ClientAccessSelectFields pins the exact select projection for
// the four client-access read tools against the shipped YAML. `want` is the
// complete field list per tool, compared both directions — so dropping OR
// adding a field fails the test, and the projection can't silently drift from
// the docs + CHANGELOG contract. The password guard is the load-bearing one: a
// monitor read must never request a credential field. It also asserts the
// readOnly annotation, which is what keeps these tools out of the
// enable_write_tools gate — a dropped annotation would make them vanish when
// writes are disabled, despite the advertised always-on contract.
func TestLoadTools_ClientAccessSelectFields(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	cases := []struct {
		tool, step string
		want       []string
	}{
		{"list-client-usernames", "clientUsernames", []string{
			"aclProfileName", "clientProfileName", "clientUsername", "dynamic",
			"enabled", "guaranteedEndpointPermissionOverrideEnabled", "msgVpnName",
			"subscriptionManagerEnabled",
		}},
		{"get-client-username", "clientUsername", []string{
			"aclProfileName", "clientProfileName", "clientUsername", "dynamic",
			"enabled", "guaranteedEndpointPermissionOverrideEnabled", "msgVpnName",
			"subscriptionManagerEnabled",
		}},
		{"list-client-profiles", "clientProfiles", []string{
			"allowGuaranteedEndpointCreateEnabled", "allowGuaranteedMsgReceiveEnabled",
			"allowGuaranteedMsgSendEnabled", "clientProfileName",
			"maxConnectionCountPerClientUsername", "maxEndpointCountPerClientUsername",
			"maxSubscriptionCount", "msgVpnName",
		}},
		{"get-client-profile", "clientProfile", []string{
			"allowBridgeConnectionsEnabled", "allowGuaranteedEndpointCreateDurability",
			"allowGuaranteedEndpointCreateEnabled", "allowGuaranteedMsgReceiveEnabled",
			"allowGuaranteedMsgSendEnabled", "allowSharedSubscriptionsEnabled",
			"allowTransactedSessionsEnabled", "clientProfileName", "compressionEnabled",
			"elidingEnabled", "maxConnectionCountPerClientUsername",
			"maxEffectiveEndpointCount", "maxEffectiveRxFlowCount",
			"maxEffectiveSubscriptionCount", "maxEffectiveTransactedSessionCount",
			"maxEffectiveTransactionCount", "maxEffectiveTxFlowCount", "maxEgressFlowCount",
			"maxEndpointCountPerClientUsername", "maxIngressFlowCount", "maxSubscriptionCount",
			"maxTransactedSessionCount", "maxTransactionCount", "msgVpnName",
		}},
	}

	for _, tc := range cases {
		tool := findTool(tools, tc.tool)
		if tool == nil {
			t.Errorf("%s: tool not found", tc.tool)
			continue
		}
		if tool.Annotations.ReadOnly == nil || !*tool.Annotations.ReadOnly {
			t.Errorf("%s: expected readOnly: true annotation (keeps the tool out of the enable_write_tools gate)", tc.tool)
		}
		step := findStep(tool, tc.step)
		if step == nil {
			t.Errorf("%s: step %q not found", tc.tool, tc.step)
			continue
		}

		have := stepSelectFields(step)
		want := map[string]bool{}
		for _, f := range tc.want {
			want[f] = true
		}
		for f := range want {
			if !have[f] {
				t.Errorf("%s select is missing %q — part of the documented contract", tc.tool, f)
			}
		}
		for f := range have {
			if !want[f] {
				t.Errorf("%s select has undocumented field %q — update want or the YAML", tc.tool, f)
			}
		}
		// Kept as a separate, explicitly named assertion even though full
		// equality would also catch it: a monitor read must never surface a
		// credential.
		if have["password"] {
			t.Errorf("%s select requests password — a monitor read must never surface a credential", tc.tool)
		}
	}
}
