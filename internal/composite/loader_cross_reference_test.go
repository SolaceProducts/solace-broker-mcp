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

// TestListTools_ReferenceTheirDetailTool pins SOL-152122: every list-X tool's
// description must name its list-then-drill-down "show details" counterpart,
// so an LLM reading only the list tool's description still discovers the
// detail tool exists. Loads the real embedded tools.yaml (not a synthetic
// fixture) so an edit that silently drops a cross-reference is caught here,
// not just by a human re-reading the YAML.
func TestListTools_ReferenceTheirDetailTool(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools error: %v", err)
	}
	descriptions := make(map[string]string, len(tools))
	for _, tool := range tools {
		descriptions[tool.Name] = tool.Description
	}

	cases := []struct {
		list   string
		detail string
	}{
		{"list-vpns", "get-vpn-status"},
		{"list-queues", "get-queue-metrics"},
		{"list-clients", "get-client-details"},
		{"list-rdps", "get-rdp-status"},
		{"list-bridges", "get-bridge-status"},
	}
	for _, tc := range cases {
		t.Run(tc.list, func(t *testing.T) {
			desc, ok := descriptions[tc.list]
			if !ok {
				t.Fatalf("tool %q not found in tools.yaml", tc.list)
			}
			if !strings.Contains(desc, tc.detail) {
				t.Errorf("%s's description does not mention %s — an LLM reading only "+
					"the list tool has no way to discover the drill-down tool exists:\n%s",
					tc.list, tc.detail, desc)
			}
		})
	}
}
