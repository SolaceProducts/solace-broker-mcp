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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ResponseCapBytes is the size, in bytes of the indented JSON text a composite
// tool returns, above which BoundResponse trims raw rows (SOL-155577).
//
// The unit is the text the calling model reads (TextContent), not the compact
// structuredContent: the text is the larger of the two copies of every result
// (1.09x to 1.47x across the shipped tools, depending on row shape), so
// bounding it bounds both. The server cannot know a calling host's token budget,
// so this is a byte proxy for it. The one failure that motivated it, list-
// queue-discards' 72,416-character response (SOL-155426), was recorded without
// saying which copy was measured: 64 KiB of text is below that figure if it was
// the text, and well below it if it was the compact form. Every shipped tool
// at its documented default stays under the cap with realistic data except the
// ones whose raw rows are the unbounded part, which is what the valve exists to
// trim. A constant, not configuration: it is a backstop for a tool that forgot
// to bound itself, not a per-deployment tuning knob — an operator who wants
// smaller responses lowers maxResults.
const ResponseCapBytes = 64 * 1024

// maxResultsAdvice is the sentence fetchPaginated appends to the "Results
// limited to N." note when maxResults cut a step short. BoundResponse removes
// it from a step it trims: asking for more rows cannot help a result that is
// already at the size cap, so the advice would send the caller on a call that
// returns the same rows.
func maxResultsAdvice() string {
	return fmt.Sprintf(" Use maxResults (up to %d) to retrieve more.", capMax)
}

// cursorAdvice is the sentence fetchPaginated appends to the "Results limited
// to N." note when it returns a nextCursor. BoundResponse removes it, and the
// cursor, from a step it trims: the cursor points past the last row fetched,
// so following it would skip the rows the cap omitted.
func cursorAdvice() string {
	return fmt.Sprintf(" More exist: pass nextCursor as cursor to get the next page (maxResults up to %d per page).", capMax)
}

// boundItemIndent is the indentation of a row inside "data" in the indented
// text: result object (2) + step object (4) + data array (6). It only has to be
// close; the exact size is re-measured before anything is trimmed for good.
const boundItemIndent = "      "

// BoundReport describes what BoundResponse did to one result.
type BoundReport struct {
	// Truncated is true when rows were omitted.
	Truncated bool
	// Unbounded is true when the result is over the cap but trimming every
	// raw row could not bring it under: the excess is in a handler's summary
	// or in non-list fields. The result is left as it was.
	Unbounded bool
	// OmittedItems is the total number of rows omitted across all steps.
	OmittedItems int
	// TrimmedSteps lists, in sorted order, the step IDs that lost rows.
	TrimmedSteps []string
}

type boundCandidate struct {
	id    string
	step  map[string]any
	items []any
	sizes []int
	keep  int
	bytes int
}

// textSize is the length of v as the manager sends it in TextContent. The
// manager indents the compact marshal, which json.MarshalIndent documents as
// producing the same bytes, so the two stay in step.
func textSize(v any) (int, bool) {
	b, err := json.MarshalIndent(v, "", "  ")
	return len(b), err == nil
}

// BoundResponse trims raw rows from out, in place, until the indented text of
// the result fits under limit. size is that text's current length, which the
// caller has already measured; it is not recomputed here, so the common case
// (size <= limit) costs nothing.
//
// Only a raw step's "data" array is trimmed: those are the SEMP rows whose
// count and width the server does not control. For a postProcess tool the
// top-level "summary" is the handler's output, not a step, and is never
// touched whatever it contains — it is computed before this runs and, by the
// postprocess package's convention, bounds itself. For a collect tool every
// key is a step, including one named "summary" (the loader reserves that ID
// only for postProcess), so strategy decides which reading applies.
//
// Rows are removed from the tail of the largest array first, one at a time, so
// the arrays shrink toward equal size and the same input always yields the same
// output. A first pass picks rows from each row's size; the result is then
// built and measured exactly, and rows are removed until it fits, so the final
// text is under limit without padding for what was added. Every trimmed step
// gets "truncated": true, "omittedItems", and a "truncatedMessage" — the keys
// the paginated fetch already uses — so the caller is told, never left to infer
// it from a short list. A step's "data" stays an array even when every row is
// omitted.
//
// If emptying every array still would not fit, nothing is changed and the
// report says Unbounded. That is decided on the exact size of the emptied
// result, not on an estimate.
//
// Step maps are copied before being modified, since they are shared with the
// executor's step results.
func BoundResponse(strategy ResultStrategy, out map[string]any, size, limit int) BoundReport {
	return boundResponse(strategy, nil, out, size, limit)
}

// BoundToolResponse is BoundResponse for a result of tool. It also knows which
// steps are resumable: a trimmed resumable step's note says how to page on
// without skipping the omitted rows, even on a page that had no nextCursor
// (the last page of a scan, or a first page that reached the end).
func BoundToolResponse(tool CompositeTool, out map[string]any, size, limit int) BoundReport {
	resumable := map[string]bool{}
	for _, s := range tool.Steps {
		if s.Resumable {
			resumable[s.ID] = true
		}
	}
	return boundResponse(tool.Result, resumable, out, size, limit)
}

func boundResponse(strategy ResultStrategy, resumable map[string]bool, out map[string]any, size, limit int) BoundReport {
	if size <= limit {
		return BoundReport{}
	}
	hasSummary := strategy.Strategy == "postProcess"

	ids := make([]string, 0, len(out))
	for id := range out {
		if hasSummary && id == "summary" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var cands []*boundCandidate
	for _, id := range ids {
		step, ok := out[id].(map[string]any)
		if !ok {
			continue
		}
		items, ok := step["data"].([]any)
		if !ok || len(items) == 0 {
			continue
		}
		c := &boundCandidate{id: id, step: step, items: items, sizes: make([]int, len(items)), keep: len(items)}
		measurable := true
		for i, item := range items {
			b, err := json.MarshalIndent(item, boundItemIndent, "  ")
			if err != nil {
				measurable = false
				break
			}
			// The row plus the ",\n" and indent that separate it from the next.
			c.sizes[i] = len(b) + 2 + len(boundItemIndent)
			c.bytes += c.sizes[i]
		}
		if measurable {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return BoundReport{Unbounded: true}
	}

	// build returns a copy of out with each candidate cut to its keep count.
	// out itself is not touched until the final size is known.
	build := func() map[string]any {
		cp := make(map[string]any, len(out))
		for k, v := range out {
			cp[k] = v
		}
		for _, c := range cands {
			if c.keep < len(c.items) {
				cp[c.id] = trimmedStep(c, limit, hasSummary, resumable[c.id])
			}
		}
		return cp
	}
	// shave drops one row from the largest candidate that still has rows and
	// returns the bytes that frees, or -1 when every array is already empty.
	shave := func() int {
		var pick *boundCandidate
		for _, c := range cands {
			if c.keep > 0 && (pick == nil || c.bytes > pick.bytes) {
				pick = c
			}
		}
		if pick == nil {
			return -1
		}
		pick.keep--
		pick.bytes -= pick.sizes[pick.keep]
		return pick.sizes[pick.keep]
	}

	// Feasibility, exactly: the emptied result, with the metadata that would
	// really be added, either fits or nothing can help.
	saved := make([]int, len(cands))
	for i, c := range cands {
		saved[i], c.keep = c.keep, 0
	}
	if emptied, ok := textSize(build()); !ok || emptied > limit {
		return BoundReport{Unbounded: true}
	}
	for i, c := range cands {
		c.keep = saved[i]
	}

	// First pass, from row sizes alone. It ignores the keys that will be added,
	// so it can stop a little too early; the exact pass below finishes the job.
	for excess := size - limit; excess > 0; {
		freed := shave()
		if freed < 0 {
			break
		}
		excess -= freed
	}
	// Exact pass: build, measure, and shave the measured excess until it fits.
	// The emptied result fits (checked above), so this terminates.
	var trial map[string]any
	for {
		trial = build()
		got, _ := textSize(trial)
		if got <= limit {
			break
		}
		for excess := got - limit; excess > 0; {
			freed := shave()
			if freed < 0 {
				break
			}
			excess -= freed
		}
	}

	var report BoundReport
	for _, c := range cands {
		omitted := len(c.items) - c.keep
		if omitted == 0 {
			continue
		}
		out[c.id] = trial[c.id]
		report.Truncated = true
		report.OmittedItems += omitted
		report.TrimmedSteps = append(report.TrimmedSteps, c.id)
	}
	return report
}

// trimmedStep returns a copy of c's step cut to c.keep rows and flagged. The
// data slice is always non-nil, so a step with every row omitted serializes as
// [] and not null. A nextCursor is removed: it resumes after the last row
// fetched, not the last row kept. resumable is true when the tool declares the
// step resumable; a step carrying a nextCursor is treated as resumable too.
func trimmedStep(c *boundCandidate, limit int, hasSummary, resumable bool) map[string]any {
	trimmed := make(map[string]any, len(c.step)+3)
	for k, v := range c.step {
		trimmed[k] = v
	}
	_, hadCursor := trimmed["nextCursor"]
	delete(trimmed, "nextCursor")
	trimmed["data"] = append(make([]any, 0, c.keep), c.items[:c.keep]...)
	trimmed["truncated"] = true
	trimmed["omittedItems"] = len(c.items) - c.keep
	prev, _ := c.step["truncatedMessage"].(string)
	trimmed["truncatedMessage"] = boundNote(prev, c.keep, len(c.items), limit, hasSummary, resumable || hadCursor)
	return trimmed
}

// boundNote builds the step's truncatedMessage: any earlier note, minus advice
// the size cap makes wrong, followed by the size note. For a resumable step it
// adds how to page on without skipping rows, unless no rows were kept:
// maxResults below 1 falls back to the default, so no smaller call exists to
// suggest.
func boundNote(prev string, kept, total, limit int, hasSummary, resumable bool) string {
	note := boundMessage(kept, total, limit, hasSummary)
	if resumable && kept > 0 {
		note += fmt.Sprintf(" To page on without skipping them, repeat this call with the same cursor (none for the first page) and maxResults at most %d; its nextCursor starts right after the rows returned.", kept)
	}
	prev = strings.TrimSuffix(prev, maxResultsAdvice())
	prev = strings.TrimSuffix(prev, cursorAdvice())
	if prev == "" {
		return note
	}
	return prev + " " + note
}

// boundMessage is the text the calling agent reads. It carries only counts
// and the cap — never a caller-supplied value — and says what to do next.
func boundMessage(kept, total, limit int, hasSummary bool) string {
	msg := fmt.Sprintf("Response size limit reached: this step returned %d of %d rows (%d omitted) to stay under %d bytes.",
		kept, total, total-kept, limit)
	if hasSummary {
		msg += " The summary was computed before the rows were omitted and still covers every row fetched."
	}
	return msg + " The omitted rows are not in this response; to see specific ones, narrow the query (for example to one queue, client or Message VPN)."
}
