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
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// ltsSpecDir holds the SEMPv2 specs of the oldest supported LTS broker (10.8.1).
const ltsSpecDir = "../semp/sempv2/specs/testdata/10.8.1.599"

// ltsSpecViolations returns one message per operation, query parameter or
// select attribute a tool step uses that the LTS spec lacks. cur is the
// embedded spec; it decides which step args are sent as query parameters
// (the client drops the rest, see buildRequest).
func ltsSpecViolations(tools []CompositeTool, cur, lts map[string]*sempv2.Operation) []string {
	var out []string
	for _, tool := range tools {
		for _, step := range tool.Steps {
			where := "tool " + tool.Name + " step " + step.ID + ": "
			op := lts[step.Operation]
			if op == nil {
				out = append(out, where+"operation "+step.Operation+" missing")
				continue
			}

			query := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In == "query" {
					query[p.Name] = true
				}
			}
			if curOp := cur[step.Operation]; curOp != nil {
				for _, p := range curOp.Parameters {
					if _, used := step.Args[p.Name]; used && p.In == "query" && !query[p.Name] {
						out = append(out, where+"query parameter "+p.Name+" missing")
					}
				}
			}
			if len(step.Select) > 0 && !query["select"] {
				out = append(out, where+"query parameter select missing")
			}
			if step.FollowPages && !query["cursor"] {
				out = append(out, where+"query parameter cursor missing")
			}

			fields := append([]string(nil), step.Select...)
			if inline, ok := step.Args["select"]; ok {
				for _, f := range strings.Split(inline, ",") {
					fields = append(fields, strings.TrimSpace(f))
				}
			}
			for _, f := range fields {
				if _, ok := op.ResponseFields[f]; !ok {
					out = append(out, where+"select attribute "+f+" missing from "+step.Operation)
				}
			}
		}
	}
	return out
}

// TestLoadTools_LTSSpecCompat fails when a tool in tools.yaml uses an
// operation, query parameter or select attribute the 10.8.1 LTS broker does
// not support. Write-body attributes newer than 10.8.1 are only logged: the
// LLM supplies them, and the broker rejects them at call time.
func TestLoadTools_LTSSpecCompat(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools(definitions.FS): %v", err)
	}
	cur, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs(specs.FS): %v", err)
	}
	lts, err := sempv2.ParseSpecs(os.DirFS(ltsSpecDir))
	if err != nil {
		t.Fatalf("ParseSpecs(%s): %v", ltsSpecDir, err)
	}

	t.Run("embedded-tools", func(t *testing.T) {
		for _, v := range ltsSpecViolations(tools, cur, lts) {
			t.Error(v)
		}
	})

	// The operation and attribute below exist in the embedded 10.26.8 spec but
	// not in 10.8.1.
	t.Run("rejects-newer-operation", func(t *testing.T) {
		tool := CompositeTool{Name: "probe", Steps: []Step{{ID: "s", Operation: "monitor/getMsgVpnDmrExportFilters"}}}
		if got := ltsSpecViolations([]CompositeTool{tool}, cur, lts); len(got) != 1 {
			t.Errorf("violations = %q, want one for the operation", got)
		}
	})
	t.Run("rejects-newer-select", func(t *testing.T) {
		tool := CompositeTool{Name: "probe", Steps: []Step{{
			ID: "s", Operation: "monitor/getMsgVpnQueue",
			Select: []string{"queueName", "respectDmqEligibleEnabled"},
		}}}
		if got := ltsSpecViolations([]CompositeTool{tool}, cur, lts); len(got) != 1 {
			t.Errorf("violations = %q, want one for respectDmqEligibleEnabled", got)
		}
	})
	t.Run("rejects-newer-inline-select", func(t *testing.T) {
		tool := CompositeTool{Name: "probe", Steps: []Step{{
			ID: "s", Operation: "monitor/getMsgVpnQueue",
			Args: map[string]string{"msgVpnName": "v", "select": "queueName, respectDmqEligibleEnabled"},
		}}}
		if got := ltsSpecViolations([]CompositeTool{tool}, cur, lts); len(got) != 1 {
			t.Errorf("violations = %q, want one for respectDmqEligibleEnabled", got)
		}
	})

	t.Run("newer-write-body-attributes", func(t *testing.T) {
		for _, tool := range tools {
			for _, step := range tool.Steps {
				curOp, ltsOp := cur[step.Operation], lts[step.Operation]
				if curOp == nil || ltsOp == nil || curOp.Method == "GET" {
					continue
				}
				var newer []string
				for f := range curOp.BodyFields {
					if !ltsOp.BodyFields[f] {
						newer = append(newer, f)
					}
				}
				if len(newer) > 0 {
					sort.Strings(newer)
					t.Logf("%s (%s): not in 10.8.1: %s", tool.Name, step.Operation, strings.Join(newer, ", "))
				}
			}
		}
	})
}
