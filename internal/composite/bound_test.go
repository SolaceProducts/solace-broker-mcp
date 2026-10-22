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

package composite_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
)

// rows builds n rows of roughly width bytes each, distinct per index so a
// wrong prefix is detectable.
func rows(n, width int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"queueName": fmt.Sprintf("q-%04d-%s", i, strings.Repeat("x", width))}
	}
	return out
}

func sizeOf(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func rowCount(t *testing.T, out map[string]any, step string) int {
	t.Helper()
	return len(out[step].(map[string]any)["data"].([]any))
}

// The cap is inclusive: a result exactly at the cap is untouched, one byte
// over is trimmed. Each side is asserted against a positive control so a
// guard that never fires, or always fires, fails here.
func TestBoundResponse_Boundary(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{"queues": map[string]any{"data": rows(40, 100), "truncated": false}}
	}
	size := sizeOf(t, build())

	for _, tc := range []struct {
		name      string
		limit     int
		wantTrim  bool
		wantAsIs  bool
		wantUnbnd bool
	}{
		{"at the cap", size, false, true, false},
		{"over the cap by one byte", size - 1, true, false, false},
		{"far under the cap", size * 10, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := build()
			rep := composite.BoundResponse(out, size, tc.limit)
			if rep.Truncated != tc.wantTrim {
				t.Fatalf("Truncated = %v, want %v (report %+v)", rep.Truncated, tc.wantTrim, rep)
			}
			if rep.Unbounded != tc.wantUnbnd {
				t.Fatalf("Unbounded = %v, want %v", rep.Unbounded, tc.wantUnbnd)
			}
			if tc.wantAsIs && !reflect.DeepEqual(out, build()) {
				t.Fatal("result was modified although it was within the cap")
			}
		})
	}
}

// "Just over" must land under the cap for real, measured on the marshaled
// result, not on the function's own estimate — across a sweep of limits so an
// off-by-one in the reserve shows up at some limit.
func TestBoundResponse_ResultFitsUnderTheCap(t *testing.T) {
	base := map[string]any{
		"vpn":    map[string]any{"data": map[string]any{"msgVpnName": "default"}},
		"queues": map[string]any{"data": rows(300, 80), "truncated": true, "truncatedMessage": "Results limited to 300."},
		"extras": map[string]any{"data": rows(120, 200)},
	}
	size := sizeOf(t, base)
	for limit := 3000; limit < size; limit += 997 {
		out := map[string]any{
			"vpn":    base["vpn"],
			"queues": base["queues"],
			"extras": base["extras"],
		}
		rep := composite.BoundResponse(out, size, limit)
		if rep.Unbounded {
			t.Fatalf("limit %d: reported unbounded", limit)
		}
		if got := sizeOf(t, out); got > limit {
			t.Fatalf("limit %d: result is %d bytes after bounding", limit, got)
		}
		if !rep.Truncated || rep.OmittedItems == 0 {
			t.Fatalf("limit %d: nothing omitted: %+v", limit, rep)
		}
	}
}

// The agent must be told: a flag, how many rows were dropped, and how to narrow
// the query — in the keys paginated truncation already uses.
func TestBoundResponse_TruncationIsVisible(t *testing.T) {
	out := map[string]any{"queues": map[string]any{"data": rows(100, 100)}}
	size := sizeOf(t, out)
	rep := composite.BoundResponse(out, size, size/2)

	step := out["queues"].(map[string]any)
	kept := len(step["data"].([]any))
	if step["truncated"] != true {
		t.Fatalf("truncated = %v, want true", step["truncated"])
	}
	if step["omittedItems"] != 100-kept {
		t.Fatalf("omittedItems = %v, want %d", step["omittedItems"], 100-kept)
	}
	if rep.OmittedItems != 100-kept || !reflect.DeepEqual(rep.TrimmedSteps, []string{"queues"}) {
		t.Fatalf("report %+v does not match the step (kept %d)", rep, kept)
	}
	msg, _ := step["truncatedMessage"].(string)
	for _, want := range []string{"Response size limit reached", fmt.Sprintf("%d of 100 rows", kept), "omitted rows are not in this response", "narrow the query"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "summary") {
		t.Errorf("message %q mentions a summary the result does not have", msg)
	}
}

// A postProcess result keeps its summary untouched, and the message says the
// summary still covers every row.
func TestBoundResponse_SummaryUntouchedAndNamed(t *testing.T) {
	summary := map[string]any{"scanned": 100, "downCount": 3}
	out := map[string]any{"queues": map[string]any{"data": rows(100, 100)}, "summary": summary}
	composite.BoundResponse(out, sizeOf(t, out), 4000)

	if !reflect.DeepEqual(out["summary"], map[string]any{"scanned": 100, "downCount": 3}) {
		t.Fatalf("summary changed: %v", out["summary"])
	}
	msg := out["queues"].(map[string]any)["truncatedMessage"].(string)
	if !strings.Contains(msg, "summary was computed before the rows were omitted") {
		t.Errorf("message %q does not say the summary is still complete", msg)
	}
}

// A paginated step that already carries a maxResults message keeps it; the
// size note is appended rather than replacing it.
func TestBoundResponse_PreservesExistingTruncationMessage(t *testing.T) {
	out := map[string]any{"queues": map[string]any{
		"data": rows(100, 100), "truncated": true, "truncatedMessage": "Results limited to 100. Use maxResults (up to 500) to retrieve more.",
	}}
	composite.BoundResponse(out, sizeOf(t, out), 3000)
	msg := out["queues"].(map[string]any)["truncatedMessage"].(string)
	if !strings.HasPrefix(msg, "Results limited to 100.") || !strings.Contains(msg, "Response size limit reached") {
		t.Fatalf("message = %q, want the original followed by the size note", msg)
	}
}

// Same input, same output: the order rows are dropped in, and which step they
// come from, never depends on map iteration order.
func TestBoundResponse_Deterministic(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"a": map[string]any{"data": rows(80, 90)},
			"b": map[string]any{"data": rows(80, 90)},
			"c": map[string]any{"data": rows(80, 90)},
		}
	}
	size := sizeOf(t, build())
	first := build()
	composite.BoundResponse(first, size, size/3)
	want, _ := json.Marshal(first)
	for i := 0; i < 50; i++ {
		again := build()
		composite.BoundResponse(again, size, size/3)
		got, _ := json.Marshal(again)
		if string(got) != string(want) {
			t.Fatalf("run %d produced different output", i)
		}
	}
}

// The largest array gives up rows first, so one huge step does not leave a
// small sibling empty. The first rows are the ones kept.
func TestBoundResponse_TrimsLargestFirstAndKeepsThePrefix(t *testing.T) {
	big, small := rows(200, 100), rows(5, 100)
	out := map[string]any{"big": map[string]any{"data": big}, "small": map[string]any{"data": small}}
	size := sizeOf(t, out)
	rep := composite.BoundResponse(out, size, size/2)

	if rowCount(t, out, "small") != 5 {
		t.Errorf("small step lost rows: %d left", rowCount(t, out, "small"))
	}
	kept := out["big"].(map[string]any)["data"].([]any)
	if len(kept) >= 200 || !reflect.DeepEqual(kept, big[:len(kept)]) {
		t.Errorf("big step: kept %d rows, want a strict prefix of the original", len(kept))
	}
	if !reflect.DeepEqual(rep.TrimmedSteps, []string{"big"}) {
		t.Errorf("TrimmedSteps = %v, want [big]", rep.TrimmedSteps)
	}
	if _, set := out["small"].(map[string]any)["truncated"]; set {
		t.Error("untrimmed step was flagged truncated")
	}
}

// When the excess is not in raw rows — a bloated summary — trimming cannot help
// and must not run: the result is untouched and the report says so.
func TestBoundResponse_UnboundedWhenRowsCannotFixIt(t *testing.T) {
	huge := strings.Repeat("r", 5000)
	out := map[string]any{
		"queues":  map[string]any{"data": rows(3, 10)},
		"summary": map[string]any{"byReason": map[string]any{huge: 1}},
	}
	before, _ := json.Marshal(out)
	rep := composite.BoundResponse(out, len(before), 2000)

	if !rep.Unbounded || rep.Truncated || rep.OmittedItems != 0 {
		t.Fatalf("report = %+v, want Unbounded only", rep)
	}
	after, _ := json.Marshal(out)
	if string(after) != string(before) {
		t.Fatal("an unfixable result was modified anyway")
	}
}

// Shapes with nothing to trim report Unbounded rather than panicking.
func TestBoundResponse_NothingTrimmable(t *testing.T) {
	for name, out := range map[string]map[string]any{
		"empty":            {},
		"no data array":    {"step": map[string]any{"data": map[string]any{"a": strings.Repeat("x", 500)}}},
		"empty array":      {"step": map[string]any{"data": []any{}}},
		"step not a map":   {"step": "scalar-bigger-than-the-limit-by-a-lot"},
		"data not a slice": {"step": map[string]any{"data": "text"}},
	} {
		t.Run(name, func(t *testing.T) {
			rep := composite.BoundResponse(out, 10_000, 10)
			if !rep.Unbounded || rep.Truncated {
				t.Fatalf("report = %+v, want Unbounded", rep)
			}
		})
	}
}

// Step maps are shared with the executor's step results; trimming must not
// write through to them.
func TestBoundResponse_DoesNotMutateTheSharedStepMap(t *testing.T) {
	step := map[string]any{"data": rows(60, 100)}
	out := map[string]any{"queues": step}
	composite.BoundResponse(out, sizeOf(t, out), 2000)

	if len(step["data"].([]any)) != 60 {
		t.Errorf("shared step's rows were trimmed in place: %d left", len(step["data"].([]any)))
	}
	if _, set := step["truncated"]; set {
		t.Error("shared step was flagged in place")
	}
}

// Nothing the caller sent can reach the message: it is built from counts and
// the cap alone. A row carrying a recognisable value must not appear in it.
func TestBoundResponse_MessageHasNoRowOrCallerContent(t *testing.T) {
	const marker = "CALLER-SUPPLIED-MARKER-9f3a"
	data := rows(100, 100)
	data[0].(map[string]any)["queueName"] = marker
	out := map[string]any{"queues": map[string]any{"data": data}}
	composite.BoundResponse(out, sizeOf(t, out), 3000)

	msg := out["queues"].(map[string]any)["truncatedMessage"].(string)
	if strings.Contains(msg, marker) {
		t.Fatalf("message %q echoes row content", msg)
	}
}
