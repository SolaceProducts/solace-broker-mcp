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
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// SOL-156509: the operation audit record's request_sent says whether "the
// write" reached the broker — one bit, which is only an honest answer while a
// destructive tool makes at most one write. A second write step (or a write
// that fans out per row) would make a cancelled or failed call a partial
// apply, which the record cannot express. Rather than design for that now,
// this test fails the day a destructive composite declares one.

// destructiveMultiWriteViolations names every destructive tool with more than
// one write. A write step is one whose operation resolves to a non-GET method
// in the embedded spec; a write step with forEach counts as many writes. An
// operation the spec does not know is reported too, so the check fails closed.
func destructiveMultiWriteViolations(tools []CompositeTool, ops map[string]*sempv2.Operation) []string {
	var out []string
	for _, tool := range tools {
		if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
			continue
		}
		writes := 0
		for _, step := range tool.Steps {
			op, ok := ops[step.Operation]
			if !ok {
				out = append(out, fmt.Sprintf("%s: step %s: operation %q not in the spec", tool.Name, step.ID, step.Operation))
				continue
			}
			if op.Method == http.MethodGet {
				continue
			}
			writes++
			if step.ForEach != "" {
				writes++ // one write per row
			}
		}
		if writes > 1 {
			out = append(out, fmt.Sprintf("%s: %d writes, want at most 1", tool.Name, writes))
		}
	}
	return out
}

var (
	embeddedOpsOnce sync.Once
	embeddedOpsMap  map[string]*sempv2.Operation
	embeddedOpsErr  error
)

// embeddedOps parses the embedded spec once for both tests below: a parse
// takes seconds under -race.
func embeddedOps(t *testing.T) map[string]*sempv2.Operation {
	t.Helper()
	embeddedOpsOnce.Do(func() {
		embeddedOpsMap, embeddedOpsErr = sempv2.ParseSpecs(specs.FS)
	})
	if embeddedOpsErr != nil {
		t.Fatalf("ParseSpecs: %v", embeddedOpsErr)
	}
	return embeddedOpsMap
}

func TestEmbeddedCatalog_DestructiveToolsMakeAtMostOneWrite(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	destructive := 0
	for _, tool := range tools {
		if tool.Annotations.Destructive != nil && *tool.Annotations.Destructive {
			destructive++
		}
	}
	// Guards against a vacuous pass if the annotation stopped being parsed.
	if destructive == 0 {
		t.Fatal("no destructive tools in the catalog; the check below proves nothing")
	}
	for _, v := range destructiveMultiWriteViolations(tools, embeddedOps(t)) {
		t.Errorf("%s — request_sent cannot describe a partial apply (SOL-156509)", v)
	}
}

// The check itself, against hand-built tools, so a broken check cannot pass
// the catalog test above by accident.
func TestDestructiveMultiWriteViolations(t *testing.T) {
	ops := embeddedOps(t)
	yes, no := true, false
	read := Step{ID: "read", Operation: "monitor/getMsgVpnQueue"}
	del := Step{ID: "del", Operation: "config/deleteMsgVpnQueue"}
	del2 := Step{ID: "del2", Operation: "config/deleteMsgVpnQueueSubscription"}
	fanDel := Step{ID: "fan", Operation: "config/deleteMsgVpnQueue", ForEach: "read"}

	for _, tt := range []struct {
		name   string
		dest   *bool
		steps  []Step
		failed bool
	}{
		{"one write", &yes, []Step{del}, false},
		{"read then write", &yes, []Step{read, del}, false},
		{"two writes", &yes, []Step{del, del2}, true},
		{"fan-out write", &yes, []Step{read, fanDel}, true},
		{"unknown operation", &yes, []Step{{ID: "x", Operation: "config/noSuchOp"}}, true},
		{"two writes, not destructive", &no, []Step{del, del2}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tool := CompositeTool{Name: "t", Steps: tt.steps, Annotations: ToolAnnotations{Destructive: tt.dest}}
			got := destructiveMultiWriteViolations([]CompositeTool{tool}, ops)
			if (len(got) > 0) != tt.failed {
				t.Errorf("violations = %v, want failed=%v", got, tt.failed)
			}
		})
	}
}
