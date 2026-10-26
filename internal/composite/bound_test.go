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

var (
	collectStrategy     = composite.ResultStrategy{Strategy: "collect"}
	postProcessStrategy = composite.ResultStrategy{Strategy: "postProcess", PostProcess: "anything"}
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

// textLen is the length of v as the manager sends it in TextContent, the unit
// the cap is measured in.
func textLen(t *testing.T, v any) int {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
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
	size := textLen(t, build())

	for _, tc := range []struct {
		name     string
		limit    int
		wantTrim bool
	}{
		{"at the cap", size, false},
		{"over the cap by one byte", size - 1, true},
		{"far under the cap", size * 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := build()
			rep := composite.BoundResponse(collectStrategy, out, size, tc.limit)
			if rep.Truncated != tc.wantTrim || rep.Unbounded {
				t.Fatalf("report = %+v, want Truncated=%v and not Unbounded", rep, tc.wantTrim)
			}
			if !tc.wantTrim && !reflect.DeepEqual(out, build()) {
				t.Fatal("result was modified although it was within the cap")
			}
			if tc.wantTrim && textLen(t, out) > tc.limit {
				t.Fatalf("trimmed result is %d bytes, over the %d limit", textLen(t, out), tc.limit)
			}
		})
	}
}

// "Just over" must land under the cap for real, measured on the marshaled
// text, across a sweep of limits so an off-by-one shows up at some limit. It
// must also not over-trim: the result lands within one row of the limit, since
// the size is measured exactly rather than padded for what is added.
func TestBoundResponse_FitsUnderTheCapAndIsTight(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"vpn":    map[string]any{"data": map[string]any{"msgVpnName": "default"}},
			"queues": map[string]any{"data": rows(300, 80), "truncated": true, "truncatedMessage": "Results limited to 300."},
			"extras": map[string]any{"data": rows(120, 200)},
		}
	}
	size := textLen(t, build())
	// The widest row as it sits in the output (indented under the step's data
	// array, plus its separator) bounds how far below the limit the last
	// removal can land; the slack covers the size note changing length as the
	// counts in it change.
	rowAtDepth, err := json.MarshalIndent(rows(1, 200)[0], "      ", "  ")
	if err != nil {
		t.Fatal(err)
	}
	widest := len(rowAtDepth) + 8 + 16
	for limit := 3000; limit < size; limit += 997 {
		out := build()
		rep := composite.BoundResponse(collectStrategy, out, size, limit)
		if rep.Unbounded || !rep.Truncated || rep.OmittedItems == 0 {
			t.Fatalf("limit %d: report %+v", limit, rep)
		}
		got := textLen(t, out)
		if got > limit {
			t.Fatalf("limit %d: result is %d bytes after bounding", limit, got)
		}
		if limit-got > widest {
			t.Fatalf("limit %d: result is %d bytes, %d under the limit — more than one row (%d) was over-trimmed", limit, got, limit-got, widest)
		}
	}
}

// The agent must be told: a flag, how many rows were dropped, and how to narrow
// the query — in the keys paginated truncation already uses.
func TestBoundResponse_TruncationIsVisible(t *testing.T) {
	out := map[string]any{"queues": map[string]any{"data": rows(100, 100)}}
	size := textLen(t, out)
	rep := composite.BoundResponse(collectStrategy, out, size, size/2)

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
	composite.BoundResponse(postProcessStrategy, out, textLen(t, out), 4000)

	if !reflect.DeepEqual(out["summary"], map[string]any{"scanned": 100, "downCount": 3}) {
		t.Fatalf("summary changed: %v", out["summary"])
	}
	msg := out["queues"].(map[string]any)["truncatedMessage"].(string)
	if !strings.Contains(msg, "summary was computed before the rows were omitted") {
		t.Errorf("message %q does not say the summary is still complete", msg)
	}
}

// A postProcess summary is the handler's output, not a step, so a summary that
// happens to hold a "data" array is never trimmed — it is the one thing the
// valve promises not to touch. Only the raw step gives up rows.
func TestBoundResponse_PostProcessSummaryWithDataArrayIsNotTrimmed(t *testing.T) {
	summaryRows := rows(100, 100)
	out := map[string]any{
		"queues":  map[string]any{"data": rows(5, 50)},
		"summary": map[string]any{"data": summaryRows, "count": 100},
	}
	// Small enough that the summary's rows alone could not fit: the raw step
	// is emptied, and the result is still too big, so nothing can be done.
	rep := composite.BoundResponse(postProcessStrategy, out, textLen(t, out), 6000)

	if got := out["summary"].(map[string]any)["data"].([]any); len(got) != 100 {
		t.Fatalf("summary.data lost rows: %d left of 100", len(got))
	}
	if _, set := out["summary"].(map[string]any)["omittedItems"]; set {
		t.Fatal("summary was flagged as trimmed")
	}
	if !rep.Unbounded {
		t.Fatalf("report = %+v, want Unbounded: the only oversized part is the summary", rep)
	}

	// With room for the summary, the raw step is what gets trimmed.
	big := map[string]any{
		"queues":  map[string]any{"data": rows(100, 100)},
		"summary": map[string]any{"data": rows(3, 10), "count": 3},
	}
	rep = composite.BoundResponse(postProcessStrategy, big, textLen(t, big), 8000)
	if !rep.Truncated || !reflect.DeepEqual(rep.TrimmedSteps, []string{"queues"}) {
		t.Fatalf("report = %+v, want only the raw step trimmed", rep)
	}
	if got := big["summary"].(map[string]any)["data"].([]any); len(got) != 3 {
		t.Fatalf("summary.data changed: %d rows", len(got))
	}
}

// For a collect tool every key is a step, and the loader reserves the step ID
// "summary" only for postProcess, so a collect step named "summary" is trimmed
// like any other.
func TestBoundResponse_CollectStepNamedSummaryIsTrimmed(t *testing.T) {
	out := map[string]any{"summary": map[string]any{"data": rows(100, 100)}}
	rep := composite.BoundResponse(collectStrategy, out, textLen(t, out), 4000)

	if !rep.Truncated || rowCount(t, out, "summary") >= 100 {
		t.Fatalf("report = %+v, rows left %d: a collect step named summary was not trimmed", rep, rowCount(t, out, "summary"))
	}
	msg := out["summary"].(map[string]any)["truncatedMessage"].(string)
	if strings.Contains(msg, "summary was computed") {
		t.Errorf("message %q describes a handler summary a collect tool does not have", msg)
	}
}

// What an earlier truncation note says is kept — except advice the size cap
// makes wrong. "Use maxResults (up to 500) to retrieve more" would send the
// caller to a call that returns the same rows, so it goes; the rest of the note
// (how many rows maxResults allowed) and the other notes stay.
func TestBoundResponse_EarlierTruncationNote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prev     string
		wantKept string
		wantGone string
	}{
		{
			name:     "maxResults note loses its advice",
			prev:     "Results limited to 100. Use maxResults (up to 500) to retrieve more.",
			wantKept: "Results limited to 100.",
			wantGone: "Use maxResults",
		},
		{
			name:     "ceiling note is kept whole",
			prev:     "More results exist but the maximum limit of 500 has been reached. Not all results are shown.",
			wantKept: "More results exist but the maximum limit of 500 has been reached. Not all results are shown.",
		},
		{
			name:     "page-limit note is kept whole",
			prev:     "Pagination stopped after 50 pages; result is incomplete. This usually indicates a broker issue with a persistent nextPageUri — narrow the query (e.g., by msgVpnName or a where filter) or contact your broker administrator.",
			wantKept: "Pagination stopped after 50 pages; result is incomplete.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]any{"queues": map[string]any{"data": rows(100, 100), "truncated": true, "truncatedMessage": tc.prev}}
			composite.BoundResponse(collectStrategy, out, textLen(t, out), 3000)
			msg := out["queues"].(map[string]any)["truncatedMessage"].(string)

			if !strings.HasPrefix(msg, tc.wantKept) || !strings.Contains(msg, "Response size limit reached") {
				t.Errorf("message = %q, want the earlier note followed by the size note", msg)
			}
			if tc.wantGone != "" && strings.Contains(msg, tc.wantGone) {
				t.Errorf("message = %q still contains %q", msg, tc.wantGone)
			}
			if tc.wantGone == "" && !strings.Contains(msg, tc.prev) {
				t.Errorf("message = %q lost part of the earlier note", msg)
			}
		})
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
	size := textLen(t, build())
	first := build()
	composite.BoundResponse(collectStrategy, first, size, size/3)
	want, _ := json.Marshal(first)
	for i := 0; i < 50; i++ {
		again := build()
		composite.BoundResponse(collectStrategy, again, size, size/3)
		got, _ := json.Marshal(again)
		if string(got) != string(want) {
			t.Fatalf("run %d produced different output", i)
		}
	}
}

// The largest array gives up rows first. The big step is named so that it
// sorts LAST: draining steps in plain ID order, or always taking the first, would
// empty the small one before touching the big one, and this fails.
func TestBoundResponse_TrimsLargestFirstAndKeepsThePrefix(t *testing.T) {
	big, small := rows(200, 100), rows(5, 100)
	out := map[string]any{"z-big": map[string]any{"data": big}, "a-small": map[string]any{"data": small}}
	size := textLen(t, out)
	rep := composite.BoundResponse(collectStrategy, out, size, size/2)

	if rowCount(t, out, "a-small") != 5 {
		t.Errorf("small step lost rows: %d left of 5", rowCount(t, out, "a-small"))
	}
	kept := out["z-big"].(map[string]any)["data"].([]any)
	if len(kept) >= 200 || !reflect.DeepEqual(kept, big[:len(kept)]) {
		t.Errorf("big step: kept %d rows, want a strict prefix of the original", len(kept))
	}
	if !reflect.DeepEqual(rep.TrimmedSteps, []string{"z-big"}) {
		t.Errorf("TrimmedSteps = %v, want [z-big]", rep.TrimmedSteps)
	}
	if _, set := out["a-small"].(map[string]any)["truncated"]; set {
		t.Error("untrimmed step was flagged truncated")
	}
}

// Steps of similar size shrink together: their kept counts end within one row
// of each other, whatever their IDs sort as. A selection that always drains one
// step in ID order leaves one empty and the others whole.
func TestBoundResponse_SimilarStepsShrinkTogether(t *testing.T) {
	out := map[string]any{
		"alpha": map[string]any{"data": rows(90, 100)},
		"mid":   map[string]any{"data": rows(90, 100)},
		"zulu":  map[string]any{"data": rows(90, 100)},
	}
	size := textLen(t, out)
	composite.BoundResponse(collectStrategy, out, size, size/2)

	counts := []int{rowCount(t, out, "alpha"), rowCount(t, out, "mid"), rowCount(t, out, "zulu")}
	lo, hi := counts[0], counts[0]
	for _, c := range counts {
		lo, hi = min(lo, c), max(hi, c)
	}
	if hi-lo > 1 || hi >= 90 || lo == 0 {
		t.Fatalf("kept counts %v: want all three trimmed to within one row of each other", counts)
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
	rep := composite.BoundResponse(postProcessStrategy, out, textLen(t, out), 2000)

	if !rep.Unbounded || rep.Truncated || rep.OmittedItems != 0 {
		t.Fatalf("report = %+v, want Unbounded only", rep)
	}
	after, _ := json.Marshal(out)
	if string(after) != string(before) {
		t.Fatal("an unfixable result was modified anyway")
	}
}

// Whether trimming can help is decided on the exact size of the emptied
// result, not an estimate. Pin both sides of that boundary at the production
// cap: padding that leaves the emptied result exactly at the cap is trimmed;
// one byte more is unfixable. An estimate that pads for the keys it adds, as the
// first version did, reports the first case Unbounded and ships it oversized.
func TestBoundResponse_FeasibilityIsExactAtTheProductionCap(t *testing.T) {
	limit := composite.ResponseCapBytes
	build := func(pad int) map[string]any {
		return map[string]any{
			"summary": map[string]any{"pad": strings.Repeat("p", pad)},
			"queues":  map[string]any{"data": rows(1, 1000)},
		}
	}
	// Probe: a pad that leaves the result over the cap with the row, and under
	// it without. Emptying the row gives the exact size of the emptied result.
	probe := build(limit - 500)
	rep := composite.BoundResponse(postProcessStrategy, probe, textLen(t, probe), limit)
	if !rep.Truncated || rowCount(t, probe, "queues") != 0 {
		t.Fatalf("probe: report %+v, rows left %d", rep, rowCount(t, probe, "queues"))
	}
	emptied := textLen(t, probe)
	exactPad := limit - 500 + (limit - emptied)

	fits := build(exactPad)
	rep = composite.BoundResponse(postProcessStrategy, fits, textLen(t, fits), limit)
	if !rep.Truncated || rep.Unbounded {
		t.Fatalf("pad %d leaves the emptied result at the cap, but the report is %+v", exactPad, rep)
	}
	if got := textLen(t, fits); got != limit {
		t.Fatalf("trimmed result is %d bytes, want exactly %d", got, limit)
	}

	over := build(exactPad + 1)
	rep = composite.BoundResponse(postProcessStrategy, over, textLen(t, over), limit)
	if !rep.Unbounded || rep.Truncated || rowCount(t, over, "queues") != 1 {
		t.Fatalf("one byte past the boundary: report %+v, rows left %d; want Unbounded and untouched", rep, rowCount(t, over, "queues"))
	}
}

// A step whose every row is omitted keeps "data" as an empty array. A nil slice
// would serialize as null, which breaks a client that iterates the list and a
// schema that requires an array.
func TestBoundResponse_EveryRowOmittedLeavesAnEmptyArray(t *testing.T) {
	out := map[string]any{"queues": map[string]any{"data": rows(1, 3000)}}
	rep := composite.BoundResponse(collectStrategy, out, textLen(t, out), 1000)
	if !rep.Truncated || rowCount(t, out, "queues") != 0 {
		t.Fatalf("report %+v, rows left %d: want every row omitted", rep, rowCount(t, out, "queues"))
	}
	if _, isNil := out["queues"].(map[string]any)["data"].([]any); !isNil {
		t.Fatal("data is not a []any")
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), `"data":null`) || !strings.Contains(string(b), `"data":[]`) {
		t.Fatalf("serialized result = %s, want data to be []", b)
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
			rep := composite.BoundResponse(collectStrategy, out, 10_000, 10)
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
	composite.BoundResponse(collectStrategy, out, textLen(t, out), 2000)

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
	composite.BoundResponse(collectStrategy, out, textLen(t, out), 3000)

	msg := out["queues"].(map[string]any)["truncatedMessage"].(string)
	if strings.Contains(msg, marker) {
		t.Fatalf("message %q echoes row content", msg)
	}
}

// cursorStep is a resumable step as fetchPaginated returns it when maxResults
// cut it short: the broker's cursor and the note telling the caller to use it.
func cursorStep(data []any, cursor string) map[string]any {
	return map[string]any{
		"data":       data,
		"truncated":  true,
		"nextCursor": cursor,
		"truncatedMessage": fmt.Sprintf("Results limited to %d. More exist: pass nextCursor as cursor to get the next page (maxResults up to 500 per page).",
			len(data)),
	}
}

// A step that is not trimmed keeps its cursor and note: the valve only
// changes what it cuts.
func TestBoundResponse_CursorKeptWhenNotTrimmed(t *testing.T) {
	out := map[string]any{"queues": cursorStep(rows(40, 100), "C40")}
	want := fmt.Sprint(out)
	if rep := composite.BoundResponse(collectStrategy, out, textLen(t, out), 1<<20); rep.Truncated {
		t.Fatalf("report %+v, want nothing trimmed", rep)
	}
	if got := fmt.Sprint(out); got != want {
		t.Fatalf("untrimmed step changed:\n got %s\nwant %s", got, want)
	}
}

// When every row is omitted there is no maxResults that returns fewer rows
// (maxResults below 1 falls back to the default), so the cursor goes and no
// retry size is suggested. The cursor value itself never reaches the message.
func TestBoundResponse_EveryRowOmittedDropsCursorWithoutRetryAdvice(t *testing.T) {
	const marker = "CURSOR-MARKER-5c1e"
	out := map[string]any{"queues": cursorStep(rows(1, 3000), marker)}
	rep := composite.BoundResponse(collectStrategy, out, textLen(t, out), 1000)
	if !rep.Truncated || rowCount(t, out, "queues") != 0 {
		t.Fatalf("report %+v, rows left %d: want every row omitted", rep, rowCount(t, out, "queues"))
	}
	step := out["queues"].(map[string]any)
	if c, ok := step["nextCursor"]; ok {
		t.Errorf("nextCursor = %v kept on a step with every row omitted", c)
	}
	msg := step["truncatedMessage"].(string)
	for _, bad := range []string{"maxResults at most", "pass nextCursor", marker} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q contains %q", msg, bad)
		}
	}
}
