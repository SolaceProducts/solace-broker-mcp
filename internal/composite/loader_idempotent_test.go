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

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
)

// annotations.idempotent stopped being a client-facing hint and became a
// data-integrity control: the retry policy keys off it to decide whether a
// failed request may be replayed (see resilience.WithRetryUnsafe). Because an
// omitted annotation falls back to "replay allowed", an author who adds an
// action tool and forgets the field would get silent data destruction with a
// green build. These tests make that failure loud instead.

func TestValidateTool_ActionStepRequiresExplicitIdempotent(t *testing.T) {
	step := func() []Step {
		return []Step{{ID: "act", Operation: "action/doMsgVpnQueueDeleteMsgs"}}
	}

	tests := []struct {
		name       string
		idempotent *bool
		wantErr    bool
	}{
		{name: "omitted is rejected", idempotent: nil, wantErr: true},
		{name: "explicit false is accepted", idempotent: boolPtr(false)},
		{name: "explicit true is accepted", idempotent: boolPtr(true)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &CompositeTool{
				Name:        "t",
				Description: "d",
				Steps:       step(),
				Result:      ResultStrategy{Strategy: "collect"},
				Annotations: ToolAnnotations{Idempotent: tt.idempotent},
			}
			err := validateTool(tool)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error for an action step with no idempotent annotation, got nil")
				}
				if !strings.Contains(err.Error(), "annotations.idempotent") {
					t.Errorf("error should name the missing field, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// A monitor-only tool must not be forced to declare idempotency — the rule
// exists for action RPC, not for reads.
func TestValidateTool_MonitorStepDoesNotRequireIdempotent(t *testing.T) {
	tool := &CompositeTool{
		Name:        "t",
		Description: "d",
		Steps:       []Step{{ID: "read", Operation: "monitor/getMsgVpnQueue"}},
		Result:      ResultStrategy{Strategy: "collect"},
	}
	if err := validateTool(tool); err != nil {
		t.Fatalf("monitor-only tool should not require an idempotent annotation: %v", err)
	}
}

// TestEmbeddedDefinitions_ActionToolsDeclareIdempotent guards the shipped file
// itself. The loader rule above catches a *new* action tool that forgets the
// annotation; this catches someone deleting or flipping it on the two tools
// whose replay destroys data. Without it, removing one YAML line silently
// re-opens SOL-152400 with the whole suite still green.
func TestEmbeddedDefinitions_ActionToolsDeclareIdempotent(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	// The two shipped action tools whose replay is destructive rather than
	// merely repeated. Both must declare idempotent: false.
	wantFalse := map[string]bool{
		"delete-queue-messages": false,
		"disconnect-client":     false,
	}

	seen := make(map[string]bool)
	for _, tool := range tools {
		want, tracked := wantFalse[tool.Name]
		if !tracked {
			continue
		}
		seen[tool.Name] = true
		if tool.Annotations.Idempotent == nil {
			t.Errorf("%s must declare annotations.idempotent explicitly; the retry policy "+
				"keys off it and an omission allows a destructive replay", tool.Name)
			continue
		}
		if *tool.Annotations.Idempotent != want {
			t.Errorf("%s declares idempotent: %t, want %t", tool.Name, *tool.Annotations.Idempotent, want)
		}
	}

	for name := range wantFalse {
		if !seen[name] {
			t.Errorf("tool %q not found in the embedded definitions — if it was renamed, "+
				"update this guard rather than deleting it", name)
		}
	}
}
