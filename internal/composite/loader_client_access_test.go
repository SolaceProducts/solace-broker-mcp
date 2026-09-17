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

// TestLoadTools_ClientAccessSelectFields pins the documented select fields for
// the four client-access read tools against the shipped YAML. These fields are
// the tools' contract (docs + CHANGELOG), but the executor tests exercise
// hand-built fixtures — so without this, dropping a field from the YAML would
// fail no unit test. The `password` guard is the load-bearing one: a monitor
// read must never request a credential field.
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
			"clientUsername", "enabled", "clientProfileName", "aclProfileName",
			"guaranteedEndpointPermissionOverrideEnabled", "subscriptionManagerEnabled",
		}},
		{"get-client-username", "clientUsername", []string{
			"clientUsername", "enabled", "clientProfileName", "aclProfileName", "dynamic",
			"guaranteedEndpointPermissionOverrideEnabled", "subscriptionManagerEnabled",
		}},
		{"list-client-profiles", "clientProfiles", []string{
			"clientProfileName", "allowGuaranteedMsgSendEnabled",
			"allowGuaranteedMsgReceiveEnabled", "allowGuaranteedEndpointCreateEnabled",
			"maxSubscriptionCount",
		}},
		{"get-client-profile", "clientProfile", []string{
			"clientProfileName", "allowGuaranteedMsgSendEnabled",
			"allowGuaranteedMsgReceiveEnabled", "maxEffectiveSubscriptionCount",
		}},
	}

	for _, tc := range cases {
		tool := findTool(tools, tc.tool)
		if tool == nil {
			t.Errorf("%s: tool not found", tc.tool)
			continue
		}
		step := findStep(tool, tc.step)
		if step == nil {
			t.Errorf("%s: step %q not found", tc.tool, tc.step)
			continue
		}
		have := stepSelectFields(step)
		for _, f := range tc.want {
			if !have[f] {
				t.Errorf("%s select is missing %q — it is part of the tool's documented contract", tc.tool, f)
			}
		}
		// A monitor read must never request a credential field.
		if have["password"] {
			t.Errorf("%s select requests password — a monitor read must never surface a credential", tc.tool)
		}
	}
}
