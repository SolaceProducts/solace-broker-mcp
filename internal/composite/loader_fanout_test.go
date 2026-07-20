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
	"testing/fstest"
)

// loadYAML is a small helper to run a YAML through LoadTools. Returns the
// resulting error (nil on success) so table-driven tests can substring-match.
func loadYAML(t *testing.T, yaml string) error {
	t.Helper()
	fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}
	_, err := LoadTools(fsys, "tools.yaml")
	return err
}

// TestValidateFanOut covers the loader-level fan-out validation as a table.
// Each case is a minimal YAML shaped to hit exactly one rule; a passing case
// anchors that the shape itself is valid so error tests don't fire on unrelated
// grounds.
func TestValidateFanOut(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string // "" means expect success
	}{
		{
			name: "valid fan-out",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
        select: [msgVpnName]
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
		},
		{
			name: "forEach names a step declared later — forward ref",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
      - id: parent
        operation: monitor/getMsgVpns
    result:
      strategy: collect
`,
			wantSub: `not declared before this step`,
		},
		{
			name: "forEach cannot reference itself",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: self
        operation: monitor/getMsgVpnClients
        forEach: self
        forEachKey: msgVpnName
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
			wantSub: `forEach cannot reference the step itself`,
		},
		{
			name: "forEach + parallel is rejected",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
        select: [msgVpnName]
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        parallel: true
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
			wantSub: `forEach cannot be combined with parallel`,
		},
		{
			name: "forEach without forEachKey",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
			wantSub: `forEach requires forEachKey`,
		},
		{
			name: "forEachKey not in parent's non-empty select",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
        select: [state]
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
			wantSub: `not in parent step "parent"'s select`,
		},
		{
			name: "forEachKey not in select is allowed when parent has no select",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
		},
		{
			name: "concurrency above cap is rejected",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: parent
        operation: monitor/getMsgVpns
        select: [msgVpnName]
      - id: child
        operation: monitor/getMsgVpnClients
        forEach: parent
        forEachKey: msgVpnName
        concurrency: 999
        args:
          msgVpnName: "{{.Item.msgVpnName}}"
    result:
      strategy: collect
`,
			wantSub: `concurrency 999 out of range`,
		},
		{
			name: "fan-out fields without forEach are a config smell",
			yaml: `
tools:
  - name: t
    description: d
    steps:
      - id: solo
        operation: monitor/getMsgVpns
        forEachKey: msgVpnName
    result:
      strategy: collect
`,
			wantSub: `require forEach to be set`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := loadYAML(t, tc.yaml)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}
