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
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// resumableListQueuesTool is listQueuesTool with the queues step marked
// resumable and a cursor parameter declared, as list-queues is in tools.yaml.
func resumableListQueuesTool() CompositeTool {
	tool := listQueuesTool()
	tool.Parameters = append(tool.Parameters, ParameterDef{Name: "cursor", Type: "string"})
	tool.Steps[0].Resumable = true
	return tool
}

// The shipped list-clients definition pages like list-queues: a truncated
// result carries clients.nextCursor, and passing it back as cursor walks every
// client once, in order. The expected names come from genClient, not from the
// tool, so this fails if tools.yaml loses resumable or the cursor parameter.
func TestExecute_ShippedListClients_PagesByCursor(t *testing.T) {
	const total = 250
	tool, ce := shippedTool(t, "list-clients")
	g := &genClient{total: total}
	want := make([]any, total)
	for i := range want {
		want[i] = g.val("clientName", i)
	}

	var got []any
	cursor, pages := "", 0
	for ; pages < 10; pages++ {
		params := map[string]any{"msgVpnName": "default", "maxResults": float64(100)}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := ce.Execute(context.Background(), tool, g, params)
		if err != nil {
			t.Fatal(err)
		}
		step := res["clients"].(map[string]any)
		for _, row := range step["data"].([]any) {
			got = append(got, row.(map[string]any)["clientName"])
		}
		next, _ := step["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 2 {
		t.Errorf("finished after %d cursor follow-ups, want 2 (250 clients at 100 per page)", pages)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged %d client names, want all %d once each, in order", len(got), total)
	}
}

func runQueues(t *testing.T, tool CompositeTool, client *seqMockClient, params map[string]any) map[string]any {
	t.Helper()
	executor := NewCompositeExecutor(testOperations())
	result, err := executor.Execute(context.Background(), tool, client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return result["queues"].(map[string]any)
}

// A resumable call asks the broker for only the items still wanted, so it
// ends on a page boundary and the broker's next cursor points at item
// maxResults+1. Without the count clamp the second request asks for 100,
// the call drops 50 of them, and nextCursor would skip those 50.
func TestFetchPaginated_Resumable_EndsOnPageBoundaryWithNextCursor(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues",
		pageResult(makeQueueItemsFrom(0, 100), "c1"),
		pageResult(makeQueueItemsFrom(100, 50), "c2"),
	)

	queues := runQueues(t, resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(150),
	})

	if len(client.calls) != 2 {
		t.Fatalf("SEMP calls = %d, want 2", len(client.calls))
	}
	if got := client.calls[0].args["count"]; got != "100" {
		t.Errorf("first request count = %v, want 100", got)
	}
	if got := client.calls[1].args["count"]; got != "50" {
		t.Errorf("second request count = %v, want 50 (only the items still wanted)", got)
	}
	if got := client.calls[1].args["cursor"]; got != "c1" {
		t.Errorf("second request cursor = %v, want c1", got)
	}
	if items := queues["data"].([]any); len(items) != 150 {
		t.Errorf("len(data) = %d, want 150", len(items))
	}
	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	if queues["nextCursor"] != "c2" {
		t.Errorf("nextCursor = %v, want c2", queues["nextCursor"])
	}
	if msg, _ := queues["truncatedMessage"].(string); !strings.Contains(msg, "cursor") {
		t.Errorf("truncatedMessage = %q, want it to tell the caller to pass cursor", msg)
	}
}

func TestFetchPaginated_Resumable_SeedsFirstRequestFromCursorParam(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItemsFrom(150, 20), ""))

	queues := runQueues(t, resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"cursor":     "c2",
	})

	if got := client.calls[0].args["cursor"]; got != "c2" {
		t.Errorf("first request cursor = %v, want c2", got)
	}
	// Last page: nothing more to resume.
	if queues["truncated"] != false {
		t.Errorf("truncated = %v, want false", queues["truncated"])
	}
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent on the last page", queues["nextCursor"])
	}
	// Marks the result as a later page, so a summary over it can say so.
	if queues["resumed"] != true {
		t.Errorf("resumed = %v, want true on a call started from a cursor", queues["resumed"])
	}
}

func TestFetchPaginated_Resumable_EmptyCursorParamIsNoCursor(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(5), ""))

	queues := runQueues(t, resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"cursor":     "",
	})

	if _, ok := client.calls[0].args["cursor"]; ok {
		t.Errorf("first request cursor = %v, want absent", client.calls[0].args["cursor"])
	}
	if _, ok := queues["resumed"]; ok {
		t.Errorf("resumed = %v, want absent when the cursor is empty", queues["resumed"])
	}
}

// The last page exactly filling maxResults is not a truncation.
func TestFetchPaginated_Resumable_LastPageExactlyFills(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(100), ""))

	queues := runQueues(t, resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(100),
	})

	if queues["truncated"] != false {
		t.Errorf("truncated = %v, want false", queues["truncated"])
	}
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent", queues["nextCursor"])
	}
	if _, ok := queues["resumed"]; ok {
		t.Errorf("resumed = %v, want absent on a first page", queues["resumed"])
	}
}

// A broker that ignores count and returns more than asked would make its
// cursor skip the dropped items, so no nextCursor is offered.
func TestFetchPaginated_Resumable_OversizedPageGivesNoCursor(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(100), "c1"))

	queues := runQueues(t, resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(40),
	})

	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent (cursor would skip 60 dropped items)", queues["nextCursor"])
	}
}

func TestFetchPaginated_Resumable_PageCapGivesNoCursor(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(1), "cursor-perpetual"))
	executor := NewCompositeExecutor(testOperations())
	executor.maxPages = 3

	result, err := executor.Execute(context.Background(), resumableListQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	queues := result["queues"].(map[string]any)
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent when the page cap stopped the loop", queues["nextCursor"])
	}
}

// Other followPages tools keep today's requests and output exactly.
func TestFetchPaginated_NotResumable_Unchanged(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues",
		pageResult(makeQueueItemsFrom(0, 100), "c1"),
		pageResult(makeQueueItemsFrom(100, 100), "c2"),
	)

	queues := runQueues(t, listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(150),
		"cursor":     "ignored",
	})

	if _, ok := client.calls[0].args["cursor"]; ok {
		t.Errorf("first request cursor = %v, want absent on a non-resumable step", client.calls[0].args["cursor"])
	}
	if got := client.calls[1].args["count"]; got != "100" {
		t.Errorf("second request count = %v, want 100 (no clamp on a non-resumable step)", got)
	}
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent on a non-resumable step", queues["nextCursor"])
	}
	if _, ok := queues["resumed"]; ok {
		t.Errorf("resumed = %v, want absent on a non-resumable step", queues["resumed"])
	}
}

// The last page exactly fills maxResults with more pages behind it: the one
// case where a non-resumable step could reach a usable broker cursor.
func TestFetchPaginated_NotResumable_ExactFillGivesNoCursor(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(100), "c1"))

	queues := runQueues(t, listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(100),
	})

	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	if _, ok := queues["nextCursor"]; ok {
		t.Errorf("nextCursor = %v, want absent on a non-resumable step", queues["nextCursor"])
	}
}

func TestBuildStrictOutputSchema_ResumableStepAllowsNextCursor(t *testing.T) {
	operations := map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueues": fakeOpWithResponseFields(map[string]string{"queueName": "string"}),
	}
	doc := map[string]any{"queues": map[string]any{
		"data":       []any{},
		"truncated":  true,
		"nextCursor": "c1",
		"resumed":    true,
	}}

	resumable := CompositeTool{
		Steps:  []Step{{ID: "queues", Operation: "monitor/getMsgVpnQueues", FollowPages: true, Resumable: true}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	if r := validateAgainst(t, BuildStrictOutputSchema(resumable, operations, nil), doc); !r.Valid() {
		t.Errorf("expected nextCursor valid on a resumable step, got errors: %v", r.Errors())
	}

	plain := CompositeTool{
		Steps:  []Step{{ID: "queues", Operation: "monitor/getMsgVpnQueues", FollowPages: true}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	if r := validateAgainst(t, BuildStrictOutputSchema(plain, operations, nil), doc); r.Valid() {
		t.Error("expected nextCursor rejected on a non-resumable step")
	}
}

func TestLoadTools_ResumableRules(t *testing.T) {
	const tmpl = `
tools:
  - name: t
    description: resumable rules
    parameters:
%s
    steps:
      - id: s1
        operation: monitor/getVpn
%s
    result:
      strategy: collect
`
	const cursorParam = `      - name: cursor
        type: string`
	cases := []struct {
		name    string
		params  string
		step    string
		wantSub string // empty: must load
	}{
		{
			name:   "valid",
			params: cursorParam,
			step: `        followPages: true
        resumable: true
        args:
          count: "100"`,
		},
		{
			name:   "without followPages",
			params: cursorParam,
			step: `        resumable: true
        args:
          count: "100"`,
			wantSub: "resumable requires followPages",
		},
		{
			name: "without cursor parameter",
			params: `      - name: maxResults
        type: integer`,
			step: `        followPages: true
        resumable: true
        args:
          count: "100"`,
			wantSub: `resumable requires a "cursor" parameter`,
		},
		{
			name:   "without count",
			params: cursorParam,
			step: `        followPages: true
        resumable: true`,
			wantSub: "resumable requires args.count",
		},
		{
			name:   "templated count",
			params: cursorParam,
			step: `        followPages: true
        resumable: true
        args:
          count: "{{.Params.n}}"`,
			wantSub: "resumable requires args.count",
		},
		{
			// The executor reads cursor only as a string; any other type would
			// silently restart every call from the first page.
			name: "cursor parameter not a string",
			params: `      - name: cursor
        type: integer`,
			step: `        followPages: true
        resumable: true
        args:
          count: "100"`,
			wantSub: `resumable requires the "cursor" parameter to be type string`,
		},
		{
			// One cursor parameter cannot resume two scans.
			name:   "two resumable steps",
			params: cursorParam,
			step: `        followPages: true
        resumable: true
        args:
          count: "100"
      - id: s2
        operation: monitor/getVpn
        followPages: true
        resumable: true
        args:
          count: "100"`,
			wantSub: "step s2: resumable is already set on step s1",
		},
		{
			// Every fan-out iteration would be seeded from the same cursor.
			name:   "inside forEach",
			params: cursorParam,
			step: `        args:
          count: "100"
      - id: s2
        operation: monitor/getVpn
        forEach: s1
        forEachKey: msgVpnName
        followPages: true
        resumable: true
        args:
          count: "100"`,
			wantSub: "step s2: resumable cannot be combined with forEach",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := strings.Replace(tmpl, "%s", tc.params, 1)
			yaml = strings.Replace(yaml, "%s", tc.step, 1)
			fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}
			_, err := LoadTools(fsys, "tools.yaml")
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
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}
