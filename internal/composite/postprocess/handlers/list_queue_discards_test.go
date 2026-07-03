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

package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// queueDiscard builds a queue item with every required discard field zeroed
// out. Callers set the counters they care about via the overrides map. Every
// row also carries queueName and msgVpnName so the offender emitter has an
// identity to key on.
func queueDiscard(name, vpn string, overrides map[string]float64) map[string]any {
	q := map[string]any{
		"queueName":  name,
		"msgVpnName": vpn,
	}
	for _, f := range discardFields {
		q[f] = float64(0)
	}
	for k, v := range overrides {
		q[k] = v
	}
	return q
}

func runListQueueDiscards(t *testing.T, items []any, extraStep map[string]any) map[string]any {
	t.Helper()
	step := map[string]any{"data": items}
	for k, v := range extraStep {
		step[k] = v
	}
	got, err := ListQueueDiscards(map[string]map[string]any{listQueueDiscardsStepID: step})
	if err != nil {
		t.Fatalf("ListQueueDiscards: %v", err)
	}
	return got
}

// TestListQueueDiscards_OrderingAndCap covers descending sort, alphabetical
// tie-breaker on queueName, and the top-10 cap in a single scenario: 15
// discarding queues with a deliberate tie in the middle. The top-10 slice
// must retain the descending-then-alphabetical order.
func TestListQueueDiscards_OrderingAndCap(t *testing.T) {
	items := []any{}
	// 12 unique-total offenders (100, 99, …, 89).
	for i := range 12 {
		items = append(items, queueDiscard(fmt.Sprintf("q-%02d", i), "default", map[string]float64{
			"maxMsgSpoolUsageExceededDiscardedMsgCount": float64(100 - i),
		}))
	}
	// Two tie-rows with total=50: c-tied and a-tied. Alphabetical ⇒ a-tied first.
	items = append(items,
		queueDiscard("c-tied", "default", map[string]float64{"disabledDiscardedMsgCount": 50}),
		queueDiscard("a-tied", "default", map[string]float64{"disabledDiscardedMsgCount": 50}),
	)
	// Quiet queue — zero discards. Must not appear in offenders and must not
	// contribute to discardingQueueCount.
	items = append(items, queueDiscard("q-quiet", "default", nil))

	got := runListQueueDiscards(t, items, nil)

	if got["discardingQueueCount"] != 14 {
		t.Errorf("discardingQueueCount: got %v, want 14", got["discardingQueueCount"])
	}
	if got["scanned"] != 15 {
		t.Errorf("scanned: got %v, want 15", got["scanned"])
	}
	offenders, ok := got["topOffenderQueues"].([]map[string]any)
	if !ok {
		t.Fatalf("topOffenderQueues: got %T, want []map[string]any", got["topOffenderQueues"])
	}
	if len(offenders) != topOffenderLimit {
		t.Fatalf("topOffenderQueues length: got %d, want %d", len(offenders), topOffenderLimit)
	}
	// Descending totalDiscards; last two of top-10 should be q-08 (92) and q-09 (91).
	for i := range 10 {
		wantName := fmt.Sprintf("q-%02d", i)
		if offenders[i]["queueName"] != wantName {
			t.Errorf("offender[%d].queueName: got %v, want %s", i, offenders[i]["queueName"], wantName)
		}
	}
}

// TestListQueueDiscards_TieBreakerLandsInTopN pins the "descending, then
// alphabetical" rule at the top-10 boundary — the tied pair sits at ranks
// 12 and 13 and must be excluded, while ranks 0..9 retain their unique
// ordering. Complements the length/ordering check above.
func TestListQueueDiscards_TieBreakerAtBoundary(t *testing.T) {
	items := []any{}
	// 10 offenders with totals 100..91.
	for i := range 10 {
		items = append(items, queueDiscard(fmt.Sprintf("q-%02d", i), "default", map[string]float64{
			"disabledDiscardedMsgCount": float64(100 - i),
		}))
	}
	// A pair tied at 91 (equal to q-09). Alphabetical order puts "a-tie"
	// before "z-tie". q-09 (queueName "q-09") sorts between them
	// ("a-tie" < "q-09" < "z-tie"), so at total=91 the alphabetical order is
	// a-tie, q-09, z-tie. Cap-at-10 must keep a-tie and q-09, drop z-tie.
	items = append(items,
		queueDiscard("z-tie", "default", map[string]float64{"disabledDiscardedMsgCount": 91}),
		queueDiscard("a-tie", "default", map[string]float64{"disabledDiscardedMsgCount": 91}),
	)
	got := runListQueueDiscards(t, items, nil)
	offenders := got["topOffenderQueues"].([]map[string]any)
	if len(offenders) != 10 {
		t.Fatalf("length: got %d, want 10", len(offenders))
	}
	// Rank 9 must be a-tie (alphabetically first among the total=91 rows).
	if offenders[9]["queueName"] != "a-tie" {
		t.Errorf("rank 9: got %v, want a-tie", offenders[9]["queueName"])
	}
	// z-tie must NOT be in the top-10 despite tying on total — alphabetical
	// tiebreaker drops it.
	for _, o := range offenders {
		if o["queueName"] == "z-tie" {
			t.Errorf("z-tie should have been dropped by tiebreaker, got %+v", offenders)
		}
	}
}

// TestListQueueDiscards_FewerThanTen returns however many qualify — no
// padding, no error. Also asserts totalDiscards is the sum of every
// discardFields entry, not just the one that dominates.
func TestListQueueDiscards_FewerThanTen(t *testing.T) {
	items := []any{
		queueDiscard("q-a", "default", map[string]float64{
			"maxTtlExpiredDiscardedMsgCount":            5,
			"maxMsgSpoolUsageExceededDiscardedMsgCount": 2,
		}),
		queueDiscard("q-b", "default", map[string]float64{
			"maxRedeliveryExceededDiscardedMsgCount": 3,
		}),
		queueDiscard("q-quiet", "default", nil),
	}
	got := runListQueueDiscards(t, items, nil)
	offenders := got["topOffenderQueues"].([]map[string]any)
	if len(offenders) != 2 {
		t.Fatalf("length: got %d, want 2", len(offenders))
	}
	if offenders[0]["queueName"] != "q-a" || offenders[0]["totalDiscards"].(float64) != 7 {
		t.Errorf("offenders[0]: got %+v, want q-a total 7", offenders[0])
	}
	if offenders[1]["queueName"] != "q-b" || offenders[1]["totalDiscards"].(float64) != 3 {
		t.Errorf("offenders[1]: got %+v, want q-b total 3", offenders[1])
	}
	if got["discardingQueueCount"] != 2 {
		t.Errorf("discardingQueueCount: got %v, want 2", got["discardingQueueCount"])
	}
}

// TestListQueueDiscards_EmptyOffenders omits topOffenderQueues from the
// summary when no queue has any discards — the key is noise otherwise. Also
// asserts the empty-input contract: scanned=0, discardingQueueCount=0, no
// offender key.
func TestListQueueDiscards_EmptyOffenders(t *testing.T) {
	t.Run("all quiet", func(t *testing.T) {
		got := runListQueueDiscards(t, []any{
			queueDiscard("q-a", "default", nil),
			queueDiscard("q-b", "default", nil),
		}, nil)
		if _, present := got["topOffenderQueues"]; present {
			t.Errorf("topOffenderQueues should be omitted when all queues are quiet, got %v", got["topOffenderQueues"])
		}
		if got["discardingQueueCount"] != 0 {
			t.Errorf("discardingQueueCount: got %v, want 0", got["discardingQueueCount"])
		}
	})
	t.Run("empty list", func(t *testing.T) {
		got := runListQueueDiscards(t, []any{}, nil)
		if _, present := got["topOffenderQueues"]; present {
			t.Errorf("topOffenderQueues should be omitted for empty input")
		}
		if got["discardingQueueCount"] != 0 || got["scanned"] != 0 {
			t.Errorf("empty input: got %+v, want discardingQueueCount=0, scanned=0", got)
		}
	})
}

// TestListQueueDiscards_DominantCategoryTies covers three cases in one place:
// unambiguous max, tie broken alphabetically, and every counter equal (still
// picks the alphabetically-earliest field).
func TestListQueueDiscards_DominantCategory(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]float64
		want      string
	}{
		{
			name:      "unambiguous",
			overrides: map[string]float64{"maxTtlExpiredDiscardedMsgCount": 10, "maxMsgSpoolUsageExceededDiscardedMsgCount": 3},
			want:      "maxTtlExpiredDiscardedMsgCount",
		},
		{
			// Tie between two categories; alphabetical (over the full field
			// name) picks maxMsgSpoolUsageExceededDiscardedMsgCount over
			// maxTtlExpiredDiscardedMsgCount.
			name:      "tied — alphabetical",
			overrides: map[string]float64{"maxTtlExpiredDiscardedMsgCount": 5, "maxMsgSpoolUsageExceededDiscardedMsgCount": 5},
			want:      "maxMsgSpoolUsageExceededDiscardedMsgCount",
		},
		{
			// Every discard counter set to the same value — should pick the
			// alphabetically-earliest field in the whole set.
			name: "all equal",
			overrides: func() map[string]float64 {
				m := map[string]float64{}
				for _, f := range discardFields {
					m[f] = 1
				}
				return m
			}(),
			want: "clientProfileDeniedDiscardedMsgCount",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runListQueueDiscards(t, []any{queueDiscard("q", "default", tc.overrides)}, nil)
			offenders := got["topOffenderQueues"].([]map[string]any)
			if len(offenders) != 1 {
				t.Fatalf("expected exactly one offender, got %d", len(offenders))
			}
			if offenders[0]["dominantCategory"] != tc.want {
				t.Errorf("dominantCategory: got %v, want %s", offenders[0]["dominantCategory"], tc.want)
			}
		})
	}
}

// TestListQueueDiscards_NumericCoercion asserts numField accepts every JSON
// numeric shape (float64, int, int64, json.Number) — matches the listQueues
// contract and lets a future SEMP-client decode-mode switch pass through
// without a handler rewrite.
func TestListQueueDiscards_NumericCoercion(t *testing.T) {
	q := queueDiscard("q", "default", nil)
	q["maxTtlExpiredDiscardedMsgCount"] = int(3)
	q["maxMsgSpoolUsageExceededDiscardedMsgCount"] = int64(4)
	q["maxRedeliveryExceededDiscardedMsgCount"] = json.Number("5")
	got := runListQueueDiscards(t, []any{q}, nil)
	offenders := got["topOffenderQueues"].([]map[string]any)
	if len(offenders) != 1 {
		t.Fatalf("expected one offender, got %d", len(offenders))
	}
	if offenders[0]["totalDiscards"].(float64) != 12 {
		t.Errorf("totalDiscards: got %v, want 12 (3+4+5 across mixed numeric shapes)", offenders[0]["totalDiscards"])
	}
}

// TestListQueueDiscards_SkipRow: one queue with a bad row (nil counter or
// missing identity) is skipped rather than aborting the call. Healthy rows
// still contribute. Mirrors listQueues' skip-don't-abort contract so one bad
// row can't drop the raw list.
func TestListQueueDiscards_SkipRow(t *testing.T) {
	healthy := queueDiscard("q-healthy", "default", map[string]float64{"maxTtlExpiredDiscardedMsgCount": 7})
	cases := []struct {
		name string
		bad  map[string]any
	}{
		{
			name: "missing queueName",
			bad: func() map[string]any {
				q := queueDiscard("q-bad", "default", nil)
				delete(q, "queueName")
				return q
			}(),
		},
		{
			name: "missing msgVpnName",
			bad: func() map[string]any {
				q := queueDiscard("q-bad", "default", nil)
				delete(q, "msgVpnName")
				return q
			}(),
		},
		{
			name: "nil discard counter",
			bad: func() map[string]any {
				q := queueDiscard("q-bad", "default", nil)
				q["maxTtlExpiredDiscardedMsgCount"] = nil
				return q
			}(),
		},
		{
			name: "missing discard counter",
			bad: func() map[string]any {
				q := queueDiscard("q-bad", "default", nil)
				delete(q, "disabledDiscardedMsgCount")
				return q
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runListQueueDiscards(t, []any{tc.bad, healthy}, nil)
			if got["skipped"] != 1 {
				t.Errorf("skipped: got %v, want 1", got["skipped"])
			}
			if got["scanned"] != 2 {
				t.Errorf("scanned: got %v, want 2", got["scanned"])
			}
			offenders := got["topOffenderQueues"].([]map[string]any)
			if len(offenders) != 1 || offenders[0]["queueName"] != "q-healthy" {
				t.Errorf("healthy row lost: got %+v", offenders)
			}
		})
	}
}

// TestListQueueDiscards_TruncationSurfaced: propagate truncated from the step
// result so the LLM sees the partial-scan reality next to the offender list.
// Without this, offenders look global when they're only over the visible page.
func TestListQueueDiscards_TruncationSurfaced(t *testing.T) {
	items := []any{queueDiscard("q", "default", map[string]float64{"maxTtlExpiredDiscardedMsgCount": 1})}
	t.Run("truncated", func(t *testing.T) {
		got := runListQueueDiscards(t, items, map[string]any{"truncated": true})
		if got["truncated"] != true {
			t.Errorf("truncated: got %v, want true", got["truncated"])
		}
	})
	t.Run("not truncated omits flag", func(t *testing.T) {
		got := runListQueueDiscards(t, items, map[string]any{"truncated": false})
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false")
		}
	})
}

// TestListQueueDiscards_SkippedOmittedWhenZero keeps the common-case summary
// minimal — skipped is noise when nothing was skipped.
func TestListQueueDiscards_SkippedOmittedWhenZero(t *testing.T) {
	got := runListQueueDiscards(t, []any{queueDiscard("q", "default", nil)}, nil)
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0")
	}
}

// TestListQueueDiscards_Errors: structural problems (missing step, wrong
// data type, non-object item) hard-fail. These render the whole result
// unusable and are not recoverable via row-skipping.
func TestListQueueDiscards_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "queueDiscards" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"queueDiscards": {"data": "oops"}},
			wantSub: "queueDiscards.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"queueDiscards": {"data": []any{"not-an-object"}}},
			wantSub: "queueDiscards.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListQueueDiscards(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListQueueDiscards_ValidatorCrossCheck: RequiredFields must be a subset
// of the list-queue-discards select: clause. If a future YAML edit drops a
// counter from the select list, ValidateTool must catch it at boot rather
// than the handler silently skipping every row. The select list here is
// copy-pasted from tools.yaml; that copy is the point of the test — the
// duplication makes the invariant testable in a Go unit test without
// parsing YAML.
func TestListQueueDiscards_ValidatorCrossCheck(t *testing.T) {
	selectFields := []string{
		"clientProfileDeniedDiscardedMsgCount",
		"destinationGroupErrorDiscardedMsgCount",
		"disabledDiscardedMsgCount",
		"lowPriorityMsgCongestionDiscardedMsgCount",
		"maxMsgSizeExceededDiscardedMsgCount",
		"maxMsgSpoolUsageExceededDiscardedMsgCount",
		"maxRedeliveryExceededDiscardedMsgCount",
		"maxRedeliveryExceededToDmqFailedMsgCount",
		"maxRedeliveryExceededToDmqMsgCount",
		"maxTtlExceededDiscardedMsgCount",
		"maxTtlExpiredDiscardedMsgCount",
		"maxTtlExpiredToDmqMsgCount",
		"maxTtlExpiredToDmqFailedMsgCount",
		"msgVpnName",
		"noLocalDeliveryDiscardedMsgCount",
		"queueName",
		"xaTransactionNotSupportedDiscardedMsgCount",
	}
	if err := postprocess.ValidateTool("list-queue-discards", "listQueueDiscards",
		[]string{listQueueDiscardsStepID}, selectFields); err != nil {
		t.Errorf("ValidateTool should accept the tools.yaml select list, got %v", err)
	}
	// Dropping any single required field must be caught.
	for _, drop := range []string{"queueName", "msgVpnName", "maxTtlExpiredDiscardedMsgCount"} {
		t.Run("drop "+drop, func(t *testing.T) {
			pruned := make([]string, 0, len(selectFields)-1)
			for _, f := range selectFields {
				if f != drop {
					pruned = append(pruned, f)
				}
			}
			err := postprocess.ValidateTool("list-queue-discards", "listQueueDiscards",
				[]string{listQueueDiscardsStepID}, pruned)
			if err == nil {
				t.Errorf("ValidateTool must fail when %q is missing from select:", drop)
			}
		})
	}
}
