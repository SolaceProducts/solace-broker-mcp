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

// The response-size valve (SOL-155577), exercised through ToolManager.CallTool.
// The trimming itself is unit-tested in internal/composite/bound_test.go; what
// is pinned here is the wiring: when the manager asks for it, what it records,
// and what the caller finally receives.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

const responseBoundedFamily = "mcp_response_bounded_total"

// boundStub is a stub tool that opts in to the valve by delegating to the real
// composite.BoundResponse, as CompositeToolHandler does. A zero strategy is a
// collect tool.
type boundStub struct {
	*stubHandler
	strategy composite.ResultStrategy
}

func (b boundStub) BoundResponse(result map[string]any, size, limit int) composite.BoundReport {
	return composite.BoundResponse(b.strategy, result, size, limit)
}

func bigRows(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"queueName": fmt.Sprintf("row-%04d-%s", i, strings.Repeat("x", 90))}
	}
	return out
}

// boundManager registers handler on a manager with a real metrics provider and
// the given cap.
func boundManager(t *testing.T, handler ToolHandler, responseCap int) (*ToolManager, *metrics.Provider) {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewToolManager(newTestPool(t), WithToolMetrics(tm))
	mgr.responseCap = responseCap
	mgr.Register(handler)
	return mgr, p
}

func stubReturning(name string, result map[string]any, isError bool) *stubHandler {
	h := newStubHandler(name)
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		// A fresh copy per call: the valve edits the result in place.
		cp := make(map[string]any, len(result))
		for k, v := range result {
			cp[k] = v
		}
		return &ToolResult{StructuredContent: cp, IsError: isError}, nil
	}
	return h
}

func callBound(t *testing.T, mgr *ToolManager, tool string) *mcp.CallToolResult {
	t.Helper()
	res, err := mgr.CallTool(context.Background(), tool, map[string]any{"broker": "dev", "msgVpnName": "default"}, Identity{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

func boundLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, responseBoundedFamily+"{") {
			out = append(out, l)
		}
	}
	return out
}

func stepRows(t *testing.T, res *mcp.CallToolResult, step string) (rows int, flagged map[string]any) {
	t.Helper()
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T", res.StructuredContent)
	}
	s := sc[step].(map[string]any)
	return len(s["data"].([]any)), s
}

// textSize is the length of v as the manager sends it in TextContent — the unit
// the response-size cap is measured in.
func textSize(t *testing.T, v any) int {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func compactSize(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func resultText(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

// The cap is inclusive at the manager too: a result exactly at the cap is sent
// as built and leaves no trace; one byte over is trimmed, counted and logged.
func TestResponseBound_Boundary(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	size := textSize(t, result)

	t.Run("at the cap", func(t *testing.T) {
		logs, restore := captureSlog(t)
		defer restore()
		mgr, p := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, size)
		res := callBound(t, mgr, "list-things")

		if rows, step := stepRows(t, res, "queues"); rows != 60 || step["truncated"] != nil {
			t.Fatalf("rows=%d truncated=%v; a result at the cap must be untouched", rows, step["truncated"])
		}
		if got := boundLines(scrape(t, p)); len(got) != 0 {
			t.Errorf("counter has %v for a result within the cap", got)
		}
		if strings.Contains(logs.String(), "size cap") {
			t.Errorf("logged for a result within the cap: %s", logs.String())
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		logs, restore := captureSlog(t)
		defer restore()
		mgr, p := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, size-1)
		res := callBound(t, mgr, "list-things")

		rows, step := stepRows(t, res, "queues")
		if rows >= 60 || step["truncated"] != true || step["omittedItems"] != 60-rows {
			t.Fatalf("rows=%d truncated=%v omittedItems=%v; want rows trimmed and flagged", rows, step["truncated"], step["omittedItems"])
		}
		if got := len(resultText(res)); got > size-1 {
			t.Errorf("trimmed text is %d bytes, over the %d cap", got, size-1)
		}
		want := responseBoundedFamily + `{broker="dev",outcome="truncated",tool="list-things"} 1`
		if got := boundLines(scrape(t, p)); len(got) != 1 || got[0] != want {
			t.Errorf("series = %v, want exactly [%s]", got, want)
		}
		for _, f := range []string{`"tool":"list-things"`, `"broker":"dev"`, `"omitted_items":`, `"original_bytes":`, `"limit_bytes":`, `"trimmed_steps":["queues"]`} {
			if !strings.Contains(logs.String(), f) {
				t.Errorf("log lacks %s: %s", f, logs.String())
			}
		}
	})
}

// The cap is measured on the indented text the model reads, not the compact
// structured copy: a result can be under the cap compact and well over it as
// text (1.09x to 1.47x across the shipped tools). With the cap set between the
// two sizes the result must be trimmed, and what is sent must fit as text. The
// control, a cap at the text size, leaves the same result alone.
func TestResponseBound_IsMeasuredOnTheIndentedText(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	compact, text := compactSize(t, result), textSize(t, result)
	if compact >= text {
		t.Fatalf("precondition: compact %d should be smaller than text %d", compact, text)
	}

	between := (compact + text) / 2
	mgr, p := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, between)
	res := callBound(t, mgr, "list-things")
	if rows, step := stepRows(t, res, "queues"); rows >= 60 || step["truncated"] != true {
		t.Fatalf("rows=%d truncated=%v; a result whose text is over the cap must be trimmed even though its compact form is under", rows, step["truncated"])
	}
	if got := len(resultText(res)); got > between {
		t.Errorf("text is %d bytes, over the %d cap", got, between)
	}
	if got := boundLines(scrape(t, p)); len(got) != 1 {
		t.Errorf("series = %v, want one truncated series", got)
	}

	control, _ := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, text)
	if rows, _ := stepRows(t, callBound(t, control, "list-things"), "queues"); rows != 60 {
		t.Errorf("rows = %d with the cap at the text size; the control must be untouched", rows)
	}
}

// What the model reads (TextContent) and what a structured client reads must be
// the same trimmed result, in valid JSON.
func TestResponseBound_TextContentMatchesStructured(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(80)}}
	mgr, _ := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, 4000)
	res := callBound(t, mgr, "list-things")

	text := res.Content[0].(*mcp.TextContent).Text
	var fromText map[string]any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil {
		t.Fatalf("text content is not JSON: %v", err)
	}
	want, _ := json.Marshal(res.StructuredContent)
	got, _ := json.Marshal(fromText)
	if string(got) != string(want) {
		t.Fatal("text content and structured content disagree after trimming")
	}
	if len(text) > 4000 || len(text) < 4000/2 {
		t.Errorf("text content is %d bytes; want it under the 4000 cap but not trimmed far below it", len(text))
	}
}

// Only a handler that opts in is trimmed: a native tool, or any handler without
// the interface, is sent as built even when large.
func TestResponseBound_HandlerWithoutBounderIsUntouched(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	mgr, p := boundManager(t, stubReturning("native-tool", result, false), 1000)
	res := callBound(t, mgr, "native-tool")

	if rows, _ := stepRows(t, res, "queues"); rows != 60 {
		t.Fatalf("rows = %d; a handler that did not opt in was trimmed", rows)
	}
	if got := boundLines(scrape(t, p)); len(got) != 0 {
		t.Errorf("counter has %v for a handler that did not opt in", got)
	}
}

// An error result is never trimmed: it must reach the caller whole.
func TestResponseBound_ErrorResultIsUntouched(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	mgr, p := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, true)}, 1000)
	res := callBound(t, mgr, "list-things")

	if rows, _ := stepRows(t, res, "queues"); rows != 60 {
		t.Fatalf("rows = %d; an error result was trimmed", rows)
	}
	if got := boundLines(scrape(t, p)); len(got) != 0 {
		t.Errorf("counter has %v for an error result", got)
	}
}

// Over the cap with nothing to trim: sent as built, recorded as unbounded so an
// operator can see a tool needs its own bound, and logged without any content.
func TestResponseBound_UnboundedIsRecordedNotTrimmed(t *testing.T) {
	result := map[string]any{
		"queues":  map[string]any{"data": bigRows(2)},
		"summary": map[string]any{"byReason": map[string]any{strings.Repeat("r", 3000): 1}},
	}
	logs, restore := captureSlog(t)
	defer restore()
	mgr, p := boundManager(t, boundStub{stubHandler: stubReturning("list-things", result, false)}, 1000)
	res := callBound(t, mgr, "list-things")

	if rows, step := stepRows(t, res, "queues"); rows != 2 || step["truncated"] != nil {
		t.Fatalf("rows=%d truncated=%v; an unfixable result must be left alone", rows, step["truncated"])
	}
	want := responseBoundedFamily + `{broker="dev",outcome="unbounded",tool="list-things"} 1`
	if got := boundLines(scrape(t, p)); len(got) != 1 || got[0] != want {
		t.Errorf("series = %v, want exactly [%s]", got, want)
	}
	if !strings.Contains(logs.String(), "has no rows to omit") || strings.Contains(logs.String(), "rrrrrrrr") {
		t.Errorf("log should name the condition and carry no content: %s", logs.String())
	}
}

// With metrics off the valve still trims; recording is the only thing skipped.
func TestResponseBound_MetricsDisabledStillTrims(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	mgr := NewToolManager(newTestPool(t)) // no WithToolMetrics
	mgr.responseCap = 3000
	mgr.Register(boundStub{stubHandler: stubReturning("list-things", result, false)})
	res := callBound(t, mgr, "list-things")

	if rows, step := stepRows(t, res, "queues"); rows >= 60 || step["truncated"] != true {
		t.Fatalf("rows=%d truncated=%v; trimming must not depend on metrics", rows, step["truncated"])
	}
}

// The trimmed result is validated again. A tool whose output schema forbids the
// keys the valve adds fails loudly instead of sending a result its own schema
// rejects.
func TestResponseBound_TrimmedResultIsRevalidated(t *testing.T) {
	result := map[string]any{"queues": map[string]any{"data": bigRows(60)}}
	h := stubReturning("list-things", result, false)
	h.outputSch = map[string]any{
		"type": "object",
		"additionalProperties": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"data": map[string]any{"type": "array"}},
		},
	}
	logs, restore := captureSlog(t)
	defer restore()
	mgr, p := boundManager(t, boundStub{stubHandler: h}, 3000)
	res := callBound(t, mgr, "list-things")

	if !res.IsError {
		t.Fatal("a trimmed result that violates the output schema was returned as a success")
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "output validation failed") {
		t.Errorf("error text = %q, want an output-validation failure", text)
	}
	// Nothing was sent, so no trim is recorded or logged as having happened.
	if got := boundLines(scrape(t, p)); len(got) != 0 {
		t.Errorf("counter has %v for a trim that never reached the caller", got)
	}
	if strings.Contains(logs.String(), "rows omitted") {
		t.Errorf("logged a trim that never reached the caller: %s", logs.String())
	}
}

// The default cap is the exported constant, not a value a test has to set.
func TestResponseBound_DefaultCapIsTheExportedConstant(t *testing.T) {
	if got := NewToolManager(newTestPool(t)).responseCap; got != composite.ResponseCapBytes {
		t.Fatalf("responseCap = %d, want composite.ResponseCapBytes (%d)", got, composite.ResponseCapBytes)
	}
}

// The real list-queue-subscriptions tool, the real executor and a stub SEMP
// server that pages: a queue with more subscription rows than fit under the
// cap comes back trimmed and flagged, deterministically, with nothing the
// caller supplied in the log.
func TestResponseBound_RealCompositeTool_EndToEnd(t *testing.T) {
	const totalRows = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/subscriptions") {
			fmt.Fprint(w, `{"data":{"queueName":"q1"},"meta":{"responseCode":200}}`)
			return
		}
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(off+count, totalRows)
		rows := make([]map[string]any, 0, end-off)
		for i := off; i < end; i++ {
			rows = append(rows, map[string]any{"subscriptionTopic": fmt.Sprintf("e2e/cap/%04d/%s", i, strings.Repeat("t", 180))})
		}
		meta := map[string]any{"responseCode": 200, "count": totalRows}
		if end < totalRows {
			meta["paging"] = map[string]any{"nextPageUri": fmt.Sprintf("/SEMP/v2/monitor/x?cursor=%d&count=%d", end, count)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": meta})
	}))
	t.Cleanup(srv.Close)

	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pool := semp.NewBrokerPool(writeTestConfig(t, [2]string{"dev", srv.URL}), nil)
	mgr := NewToolManagerFromComposite(pool, realTools, composite.NewCompositeExecutor(ops), WithToolMetrics(tm))

	logs, restore := captureSlog(t)
	defer restore()
	const callerQueue = "caller-queue-name-77"
	call := func() *mcp.CallToolResult {
		t.Helper()
		res, err := mgr.CallTool(context.Background(), "list-queue-subscriptions", map[string]any{
			"broker": "dev", "msgVpnName": "default", "queueName": callerQueue, "maxResults": 500,
		}, Identity{})
		if err != nil || res.IsError {
			t.Fatalf("CallTool: err=%v res=%+v", err, res)
		}
		return res
	}
	first, second := call(), call()

	rows, step := stepRows(t, first, "subscriptions")
	if rows == 0 || rows >= totalRows || step["truncated"] != true || step["omittedItems"] != totalRows-rows {
		t.Fatalf("rows=%d truncated=%v omittedItems=%v; want a trimmed, flagged page", rows, step["truncated"], step["omittedItems"])
	}
	if got := len(resultText(first)); got > composite.ResponseCapBytes {
		t.Errorf("text the model reads is %d bytes, over the %d cap", got, composite.ResponseCapBytes)
	}
	if a, b := first.Content[0].(*mcp.TextContent).Text, second.Content[0].(*mcp.TextContent).Text; a != b {
		t.Error("two identical calls returned different output")
	}
	want := responseBoundedFamily + `{broker="dev",outcome="truncated",tool="list-queue-subscriptions"} 2`
	if got := boundLines(scrape(t, p)); len(got) != 1 || got[0] != want {
		t.Errorf("series = %v, want exactly [%s]", got, want)
	}
	if strings.Contains(logs.String(), callerQueue) {
		t.Errorf("a caller-supplied value reached the log: %s", logs.String())
	}
}
