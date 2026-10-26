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
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess/postprocesstest"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// genClient answers every SEMP call from the request's own select list, so a
// shipped tool is measured with its real step shape rather than a hand-built
// approximation of it (SOL-155577). Rows are paged the way the broker pages
// (count + cursor), so the executor's real pagination and maxResults logic run.
//
// Widths model a fleet, not a lab: object names are unique per row, but
// categorical fields (profile, platform, state, failure reason) draw from a
// small vocabulary the way real brokers' do. Worst-case widths use the longest
// names and failure reasons the broker accepts, and every enabled object is
// down with a failure reason, so handlers that list failures are exercised;
// uniqueText makes every categorical value unique per row, which is the case a
// summary keyed by those values cannot bound.
type genClient struct {
	total      int
	worst      bool
	uniqueText bool
}

func (g *genClient) text(prefix string, i, n int) string {
	s := fmt.Sprintf("%s-%04d-", prefix, i)
	for len(s) < n {
		s += "x"
	}
	return s[:n]
}

func (g *genClient) categorical(field string, i, n int) string {
	if g.uniqueText {
		return g.text(field, i, n)
	}
	return g.text(field, i%8, n)
}

func (g *genClient) val(field string, i int) any {
	width := func(typical, worst int) int {
		if g.worst {
			return worst
		}
		return typical
	}
	lf := strings.ToLower(field)
	switch {
	case strings.HasSuffix(lf, "reason") || strings.HasSuffix(lf, "notice"):
		if !g.worst {
			return ""
		}
		return g.categorical(field, i, 200)
	case lf == "subscriptiontopic":
		return g.text("a/b/c/topic", i, width(40, 250))
	case lf == "msgvpnname":
		return g.text("vpn", 0, width(12, 32))
	case lf == "clientprofilename" || lf == "aclprofilename" || lf == "platform" || lf == "authenticationscheme" ||
		lf == "softwareversion" || lf == "accesstype" || strings.HasSuffix(lf, "state") || lf == "remotemsgvpnname" || lf == "remoterouter"+"name":
		return g.categorical(field, i, width(14, 32))
	case strings.HasSuffix(lf, "name") || lf == "clientaddress" || lf == "remotehost" || lf == "localtopic" ||
		lf == "remotetopic" || lf == "postrequesttarget" || lf == "bootstrapaddresslist" || lf == "clientusername":
		return g.text(field, i, width(28, 100))
	case strings.HasSuffix(lf, "up"):
		// Worst case is an outage: every enabled object is down, so the
		// handlers that list or group the failures have the most to say.
		return !g.worst
	case strings.HasSuffix(lf, "enabled") || lf == "slowsubscriber" || lf == "dynamic":
		return true
	case strings.Contains(lf, "time"):
		return int64(1700000000 + i)
	default:
		return int64(123456 + i)
	}
}

func (g *genClient) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	var fields []string
	if s, ok := args["select"].(string); ok && s != "" {
		for _, f := range strings.Split(s, ",") {
			fields = append(fields, strings.TrimSpace(f))
		}
	}
	if strings.HasSuffix(op.ID, "Subscriptions") {
		fields = []string{"subscriptionTopic"}
	}
	row := func(i int) map[string]any {
		m := make(map[string]any, len(fields))
		for _, f := range fields {
			m[f] = g.val(f, i)
		}
		return m
	}
	meta := map[string]any{"request": map[string]any{"uri": "http://broker/SEMP/v2/monitor", "method": "GET"}, "responseCode": int64(200)}
	links := map[string]any{"uri": "http://broker/SEMP/v2/monitor"}

	// A single-resource GET has no page list; a collection GET is the one
	// whose operation ID is a plural noun (getMsgVpnQueues, getMsgVpnClients).
	if !strings.HasSuffix(op.ID, "s") || strings.HasSuffix(op.ID, "Status") {
		return &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": row(0), "links": links, "meta": meta}}, nil
	}

	count := 10
	if c, ok := args["count"].(string); ok {
		count, _ = strconv.Atoi(c)
	}
	off := 0
	if c, ok := args["cursor"].(string); ok {
		off, _ = strconv.Atoi(c)
	}
	end := min(off+count, g.total)
	items := make([]any, 0, max(end-off, 0))
	for i := off; i < end; i++ {
		items = append(items, row(i))
	}
	meta["count"] = int64(g.total)
	if end < g.total {
		meta["paging"] = map[string]any{"cursorQuery": "c", "nextPageUri": fmt.Sprintf("/SEMP/v2/monitor/x?cursor=%d&count=%d", end, count)}
	}
	return &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": items, "links": links, "meta": meta}}, nil
}

// measuredCase is one shipped tool run at one input size and width model. Sizes
// are bytes of the indented text, the unit the cap is measured in.
type measuredCase struct {
	tool       string
	maxResults int
	worst      bool
	raw        int
	rep        BoundReport
	after      int
	summary    int
	summaryMap map[string]any
}

func (c measuredCase) String() string {
	return fmt.Sprintf("%s maxResults=%d worst=%v: %d bytes raw, %d after the cap", c.tool, c.maxResults, c.worst, c.raw, c.after)
}

func indentedLen(t *testing.T, v any) int {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func measureShippedTools(t *testing.T, uniqueText bool, only string) []measuredCase {
	t.Helper()
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ce := NewCompositeExecutor(ops)

	var cases []measuredCase
	for _, tool := range tools {
		if tool.Annotations.ReadOnly == nil || !*tool.Annotations.ReadOnly || (only != "" && tool.Name != only) {
			continue
		}
		declaresMax := false
		for _, p := range tool.Parameters {
			declaresMax = declaresMax || p.Name == "maxResults"
		}
		// A tool with no maxResults parameter is always fetched at the default;
		// asking for 500 of it would measure an input no caller can send.
		sizes := []int{defaultMax}
		if declaresMax {
			sizes = append(sizes, capMax)
		}
		for _, n := range sizes {
			for _, worst := range []bool{false, true} {
				params := map[string]any{
					"msgVpnName": "default", "clientName": "c1", "queueName": "q1", "restDeliveryPointName": "r",
					"bridgeName": "b", "bridgeVirtualRouter": "auto", "kafkaReceiverName": "k", "kafkaSenderName": "k",
					"clientUsername": "u", "clientProfileName": "p",
				}
				if declaresMax {
					params["maxResults"] = float64(n)
				}
				res, err := ce.Execute(context.Background(), tool, &genClient{total: n + 50, worst: worst, uniqueText: uniqueText}, params)
				if err != nil {
					t.Fatalf("%s: %v", tool.Name, err)
				}
				raw := indentedLen(t, res)
				summary, _ := res["summary"].(map[string]any)
				summarySize := 0
				if summary != nil {
					summarySize = indentedLen(t, summary)
				}
				rep := BoundResponse(tool.Result, res, raw, ResponseCapBytes)
				cases = append(cases, measuredCase{tool.Name, n, worst, raw, rep, indentedLen(t, res), summarySize, summary})
			}
		}
	}
	return cases
}

// Every shipped read tool, at its documented default and at the maxResults
// ceiling, in typical and worst-case widths, ends up under the cap — and the
// valve never has to report Unbounded. This is the CI-side half of the
// safeguard: the runtime valve trims rows, but it cannot bound a summary, so
// a handler whose summary grows with the item count fails here, before it
// ships (SOL-155577). A handler's own cap is therefore enforced by measuring
// its output, not by trusting a declaration of it.
func TestShippedReadTools_StayUnderTheResponseCap(t *testing.T) {
	cases := measureShippedTools(t, false, "")
	if len(cases) < 40 {
		t.Fatalf("measured only %d cases; the shipped read tools did not load", len(cases))
	}
	trimmed := 0
	for _, c := range cases {
		if c.rep.Unbounded {
			t.Errorf("%s: over the cap and nothing to trim — a summary or non-list field is unbounded", c)
		}
		if c.after > ResponseCapBytes {
			t.Errorf("%s: still over the %d-byte cap", c, ResponseCapBytes)
		}
		if c.raw <= ResponseCapBytes && (c.rep.Truncated || c.after != c.raw) {
			t.Errorf("%s: a result already within the cap was modified", c)
		}
		if c.rep.Truncated {
			trimmed++
		}
	}
	// Positive control: if the generator stopped producing large responses the
	// assertions above would pass vacuously.
	if trimmed == 0 {
		t.Fatal("no shipped tool exceeded the cap at its maximum input; the measurement is not exercising the valve")
	}
}

// A summary is the one part the valve cannot trim, so it must leave room: at
// worst-case widths and the maximum input, no handler's summary may take more
// than a quarter of the cap. This is the check that fails if a handler's
// bounding is removed.
func TestShippedPostProcessSummaries_AreSmallAgainstTheCap(t *testing.T) {
	const budget = ResponseCapBytes / 4
	checked := 0
	for _, c := range measureShippedTools(t, false, "") {
		if c.summary == 0 {
			continue
		}
		checked++
		if c.summary > budget {
			t.Errorf("%s: summary is %d bytes, over the %d-byte budget (a quarter of the cap)", c, c.summary, budget)
		}
	}
	if checked == 0 {
		t.Fatal("no postProcess summary was measured")
	}
}

// The Kafka status tools list each unexpectedly-down binding in their summary,
// capped (maxFailedBindingsListed). That cap only matters when bindings are
// down, so the size tests above could pass with the cap deleted if the
// generator never produced a failure. Pin that it does: in the worst-case runs
// the failure detail must be populated, and truncated at the cap, so the budget
// check is measuring the code that bounds it.
func TestShippedKafkaStatusSummaries_ExerciseTheFailureDetail(t *testing.T) {
	seen := 0
	for _, tool := range []string{"get-kafka-receiver-status", "get-kafka-sender-status"} {
		for _, c := range measureShippedTools(t, false, tool) {
			if !c.worst {
				continue
			}
			seen++
			// reflect, not a type assertion: the handler's slice and map types are
			// its own, and a change of either must not read as "no failures".
			failed := reflect.ValueOf(c.summaryMap["failedBindings"])
			byReason := reflect.ValueOf(c.summaryMap["byBindingFailureReason"])
			if !failed.IsValid() || failed.Len() == 0 || !byReason.IsValid() || byReason.Len() == 0 {
				t.Errorf("%s: failedBindings and byBindingFailureReason are %v and %v; the generator is not producing failures", c, c.summaryMap["failedBindings"], c.summaryMap["byBindingFailureReason"])
			}
			if c.summaryMap["failedBindingsTruncated"] != true {
				t.Errorf("%s: failedBindingsTruncated = %v; more bindings are down than the cap lists, so the cap must have applied", c, c.summaryMap["failedBindingsTruncated"])
			}
		}
	}
	if seen != 2 {
		t.Fatalf("measured %d worst-case Kafka status runs, want 2", seen)
	}
}

// Positive control for the two tests above: with the same machinery, a summary
// that grows with the item count is caught. Without this the "stay under the
// cap" assertions could pass because the detector is blind rather than because
// the handlers are bounded.
func TestResponseCapDetector_CatchesAnUnboundedSummary(t *testing.T) {
	postprocesstest.Register(t, "__unbounded_summary", postprocess.Handler{
		Fn: func(sr map[string]map[string]any) (map[string]any, error) {
			rows, _ := sr["queues"]["data"].([]any)
			// One summary entry per scanned row: the shape the convention forbids.
			return map[string]any{"perQueue": append([]any(nil), rows...)}, nil
		},
		RequiredSteps: []string{"queues"},
	})
	res, err := ApplyResultStrategy(ResultStrategy{Strategy: "postProcess", PostProcess: "__unbounded_summary"},
		map[string]map[string]any{"queues": {"data": (&genClient{total: 500, worst: true}).rowsFor(500, "queueName", "msgVpnName")}})
	if err != nil {
		t.Fatal(err)
	}
	raw := indentedLen(t, res)
	rep := BoundResponse(ResultStrategy{Strategy: "postProcess", PostProcess: "__unbounded_summary"}, res, raw, ResponseCapBytes)
	if !rep.Unbounded {
		t.Fatalf("an unbounded summary of %d bytes was not reported (report %+v)", raw, rep)
	}
}

func (g *genClient) rowsFor(n int, fields ...string) []any {
	out := make([]any, n)
	for i := range out {
		m := map[string]any{}
		for _, f := range fields {
			m[f] = g.val(f, i)
		}
		out[i] = m
	}
	return out
}

// The note a real paginated fetch writes when maxResults cut a step short, run
// through the valve: the "Use maxResults ... to retrieve more" advice goes (a
// call with a larger maxResults is trimmed back to the same rows), while the
// count it carried stays. Driven through the real executor so the text the
// valve strips is the text the fetch produces, not a copy of it in a test.
// list-client-usernames has no cursor, so its fetch writes the maxResults
// advice; a resumable step's cursor note is covered by
// TestBoundResponse_DropsCursorOfTrimmedStep.
func TestBoundResponse_StripsTheRealMaxResultsAdvice(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tool CompositeTool
	for _, c := range tools {
		if c.Name == "list-client-usernames" {
			tool = c
		}
	}
	res, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, &genClient{total: 150},
		map[string]any{"msgVpnName": "default", "maxResults": float64(100)})
	if err != nil {
		t.Fatal(err)
	}
	step := res["clientUsernames"].(map[string]any)
	before, _ := step["truncatedMessage"].(string)
	if !strings.Contains(before, "Use maxResults") {
		t.Fatalf("precondition: the fetch's own note is %q, want the maxResults advice", before)
	}

	rep := BoundToolResponse(tool, res, indentedLen(t, res), 20000)
	if !rep.Truncated {
		t.Fatalf("report %+v, want the step trimmed", rep)
	}
	after := res["clientUsernames"].(map[string]any)["truncatedMessage"].(string)
	if strings.Contains(after, "Use maxResults") || !strings.HasPrefix(after, "Results limited to 100. Response size limit reached") {
		t.Errorf("message = %q, want the count kept, the advice gone and the size note after it", after)
	}
	// No cursor parameter, so no advice to repeat the call with one.
	if strings.Contains(after, "maxResults at most") {
		t.Errorf("message = %q gives cursor paging advice for a tool without a cursor", after)
	}
}

// shippedTool returns the embedded definition of the named tool and an
// executor over the real SEMP specs.
func shippedTool(t *testing.T, name string) (CompositeTool, *CompositeExecutor) {
	t.Helper()
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range tools {
		if c.Name == name {
			return c, NewCompositeExecutor(ops)
		}
	}
	t.Fatalf("tool %s not found", name)
	return CompositeTool{}, nil
}

// boundLimitForCursorTests is small enough that a 500-row list-queues page in
// worst-case widths is always trimmed, and large enough to keep some rows.
const boundLimitForCursorTests = 20000

// A resumable step's nextCursor points past the last row fetched. When the
// valve trims rows off that page, following the cursor would skip them, so the
// cursor goes, the fetch's "pass nextCursor" advice goes with it, and the note
// says which maxResults returns exactly the rows kept (SOL-155415 review).
func TestBoundResponse_DropsCursorOfTrimmedStep(t *testing.T) {
	tool, ce := shippedTool(t, "list-queues")
	res, err := ce.Execute(context.Background(), tool, &genClient{total: 600, worst: true},
		map[string]any{"msgVpnName": "default", "maxResults": float64(500)})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := res["queues"].(map[string]any)["nextCursor"].(string); c == "" {
		t.Fatal("precondition: a truncated list-queues page carries nextCursor")
	}

	rep := BoundToolResponse(tool, res, indentedLen(t, res), boundLimitForCursorTests)
	if !rep.Truncated {
		t.Fatalf("report %+v, want the step trimmed", rep)
	}
	step := res["queues"].(map[string]any)
	if c, ok := step["nextCursor"]; ok {
		t.Errorf("nextCursor = %v kept on a trimmed step", c)
	}
	kept := len(step["data"].([]any))
	msg := step["truncatedMessage"].(string)
	if !strings.Contains(msg, fmt.Sprintf("maxResults at most %d", kept)) || strings.Contains(msg, "pass nextCursor") {
		t.Errorf("message = %q, want the retry advice for %d rows and no nextCursor advice", msg, kept)
	}
}

// Following the valve's advice reaches every queue exactly once, in order. The
// caller here does what the notes say: on a trimmed page it repeats the call
// with the same cursor and maxResults set to the rows kept; otherwise it passes
// nextCursor on. The expected names come from genClient, not from the tool.
func TestBoundResponse_RetryAdviceReachesEveryRow(t *testing.T) {
	const total = 600
	tool, ce := shippedTool(t, "list-queues")
	g := &genClient{total: total, worst: true}
	want := make([]any, total)
	for i := range want {
		want[i] = g.val("queueName", i)
	}

	var got []any
	cursor, maxResults, trims := "", capMax, 0
	for call := 0; call < 50 && len(got) <= total; call++ {
		params := map[string]any{"msgVpnName": "default", "maxResults": float64(maxResults)}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := ce.Execute(context.Background(), tool, g, params)
		if err != nil {
			t.Fatal(err)
		}
		BoundToolResponse(tool, res, indentedLen(t, res), boundLimitForCursorTests)
		step := res["queues"].(map[string]any)
		data := step["data"].([]any)
		if _, trimmed := step["omittedItems"]; trimmed {
			trims++
			msg := step["truncatedMessage"].(string)
			if !strings.Contains(msg, fmt.Sprintf("maxResults at most %d", len(data))) {
				t.Fatalf("trimmed page note %q gives no retry size", msg)
			}
			maxResults = len(data)
			continue
		}
		for _, row := range data {
			got = append(got, row.(map[string]any)["queueName"])
		}
		next, _ := step["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor, maxResults = next, capMax
	}
	if trims == 0 {
		t.Fatal("no page was trimmed, so the retry advice was never followed")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged %d names, want all %d once each, in order", len(got), total)
	}
}
