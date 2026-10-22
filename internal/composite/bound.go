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
)

// ResponseCapBytes is the size, in bytes of the compact JSON a composite tool
// returns, above which BoundResponse trims raw rows (SOL-155577).
//
// The server cannot know a calling host's token budget, so the cap is a byte
// proxy, set below the one size that is known to have failed: list-queue-
// discards' 72,416-character response overflowed a real host (SOL-155426).
// Every shipped tool at its documented default stays under it except the
// ones whose raw rows are the unbounded part, which is what the valve exists
// to trim. A constant, not configuration: it is a backstop for a tool that
// forgot to bound itself, not a per-deployment tuning knob — an operator who
// wants smaller responses lowers maxResults.
const ResponseCapBytes = 64 * 1024

// boundMetaReserve is the room reserved per trimmable step for the keys
// BoundResponse adds (truncated, omittedItems, and the message). It is an
// upper bound, so the trimmed result is never larger than the estimate; the
// bound tests assert the real result lands under the cap.
const boundMetaReserve = 768

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
	items []any
	sizes []int
	keep  int
	bytes int
}

// BoundResponse trims raw rows from out, in place, until the result fits under
// limit. size is the result's current compact-JSON size, which the caller has
// already measured; it is not recomputed here, so the common case (size <=
// limit) costs nothing.
//
// Only a step's "data" array is trimmed: those are the SEMP rows whose count
// and width the server does not control, and every composite step stores them
// the same way. A postProcess "summary" is never touched — it is computed
// before this runs and, by the postprocess package's convention, bounds
// itself.
//
// Rows are removed from the tail of the largest array first, one at a time,
// so the arrays shrink toward equal size and the same input always yields the
// same output. Every trimmed step gets "truncated": true, "omittedItems", and
// an appended "truncatedMessage" saying how to narrow the query — the keys the
// paginated fetch already uses for maxResults truncation — so the caller is
// told, never left to infer it from a short list. When trimming every row
// still would not fit, nothing is changed and the report says Unbounded.
//
// Step maps are copied before being modified, since they are shared with the
// executor's step results.
func BoundResponse(out map[string]any, size, limit int) BoundReport {
	if size <= limit {
		return BoundReport{}
	}

	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var cands []*boundCandidate
	estimate := size
	for _, id := range ids {
		step, ok := out[id].(map[string]any)
		if !ok {
			continue
		}
		items, ok := step["data"].([]any)
		if !ok || len(items) == 0 {
			continue
		}
		c := &boundCandidate{id: id, items: items, sizes: make([]int, len(items)), keep: len(items)}
		measurable := true
		for i, item := range items {
			b, err := json.Marshal(item)
			if err != nil {
				measurable = false
				break
			}
			c.sizes[i] = len(b)
			c.bytes += len(b)
		}
		if !measurable {
			continue
		}
		cands = append(cands, c)
		estimate += boundMetaReserve
	}

	// Feasibility first: if even emptying every array leaves the result over
	// the cap, trimming would only degrade the response for nothing.
	floor := estimate
	for _, c := range cands {
		floor -= c.bytes
	}
	if floor > limit {
		return BoundReport{Unbounded: true}
	}

	for estimate > limit {
		var pick *boundCandidate
		for _, c := range cands {
			if c.keep == 0 {
				continue
			}
			if pick == nil || c.bytes > pick.bytes {
				pick = c
			}
		}
		pick.keep--
		freed := pick.sizes[pick.keep]
		pick.bytes -= freed
		estimate -= freed
	}

	var report BoundReport
	_, hasSummary := out["summary"].(map[string]any)
	for _, c := range cands {
		omitted := len(c.items) - c.keep
		if omitted == 0 {
			continue
		}
		step := out[c.id].(map[string]any)
		trimmed := make(map[string]any, len(step)+3)
		for k, v := range step {
			trimmed[k] = v
		}
		trimmed["data"] = append([]any(nil), c.items[:c.keep]...)
		trimmed["truncated"] = true
		trimmed["omittedItems"] = omitted
		note := boundMessage(c.keep, len(c.items), limit, hasSummary)
		if prev, ok := step["truncatedMessage"].(string); ok && prev != "" {
			note = prev + " " + note
		}
		trimmed["truncatedMessage"] = note
		out[c.id] = trimmed
		report.Truncated = true
		report.OmittedItems += omitted
		report.TrimmedSteps = append(report.TrimmedSteps, c.id)
	}
	return report
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
