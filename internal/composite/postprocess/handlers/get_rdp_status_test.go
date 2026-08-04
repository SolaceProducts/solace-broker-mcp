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
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

// row builds a queueBinding/restConsumer item with the two required fields.
// Bindings and consumers share the same aggregation contract (up +
// lastFailureReason), so one helper covers both.
func row(up bool, reason string) map[string]any {
	return map[string]any{
		"up":                up,
		"lastFailureReason": reason,
	}
}

// steps assembles the two-step input the handler expects. Callers pass items
// per step; the wrapper handles the fixed step keys and the "data" nesting.
func steps(bindings, consumers []any) map[string]map[string]any {
	return map[string]map[string]any{
		rdpStatusBindingsStepID:  {"data": bindings},
		rdpStatusConsumersStepID: {"data": consumers},
	}
}

func TestGetRdpStatus_Empty(t *testing.T) {
	got, err := GetRdpStatus(steps([]any{}, []any{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bindingUpCount", "bindingDownCount", "bindingScannedCount",
		"consumerUpCount", "consumerDownCount", "consumerScannedCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
	for _, k := range []string{"byBindingLastFailureReason", "byConsumerLastFailureReason"} {
		m, ok := got[k].(map[string]int)
		if !ok {
			t.Fatalf("%s: want map[string]int, got %T", k, got[k])
		}
		if len(m) != 0 {
			t.Errorf("%s: want empty, got %v", k, m)
		}
	}
	for _, k := range []string{"bindingSkipped", "bindingTruncated", "consumerSkipped", "consumerTruncated"} {
		if _, present := got[k]; present {
			t.Errorf("%s should be omitted on empty input, got %v", k, got[k])
		}
	}
}

func TestGetRdpStatus_AllUp(t *testing.T) {
	bindings := []any{row(true, ""), row(true, "old scar")}
	consumers := []any{row(true, ""), row(true, "old scar")}
	got, err := GetRdpStatus(steps(bindings, consumers))
	if err != nil {
		t.Fatal(err)
	}
	if got["bindingUpCount"] != 2 || got["bindingDownCount"] != 0 {
		t.Errorf("binding counts wrong: %+v", got)
	}
	if got["consumerUpCount"] != 2 || got["consumerDownCount"] != 0 {
		t.Errorf("consumer counts wrong: %+v", got)
	}
	if len(got["byBindingLastFailureReason"].(map[string]int)) != 0 {
		t.Errorf("byBindingLastFailureReason must be empty when nothing is down: %v", got["byBindingLastFailureReason"])
	}
	if len(got["byConsumerLastFailureReason"].(map[string]int)) != 0 {
		t.Errorf("byConsumerLastFailureReason must be empty when nothing is down: %v", got["byConsumerLastFailureReason"])
	}
}

func TestGetRdpStatus_AllDown(t *testing.T) {
	bindings := []any{row(false, "conn refused"), row(false, "conn refused"), row(false, "500")}
	consumers := []any{row(false, "timeout"), row(false, "timeout")}
	got, err := GetRdpStatus(steps(bindings, consumers))
	if err != nil {
		t.Fatal(err)
	}
	if got["bindingUpCount"] != 0 || got["bindingDownCount"] != 3 {
		t.Errorf("binding counts wrong: %+v", got)
	}
	if got["consumerUpCount"] != 0 || got["consumerDownCount"] != 2 {
		t.Errorf("consumer counts wrong: %+v", got)
	}
	bindReason := got["byBindingLastFailureReason"].(map[string]int)
	if bindReason["conn refused"] != 2 || bindReason["500"] != 1 || len(bindReason) != 2 {
		t.Errorf("byBindingLastFailureReason: got %v, want {conn refused:2, 500:1}", bindReason)
	}
	consReason := got["byConsumerLastFailureReason"].(map[string]int)
	if consReason["timeout"] != 2 || len(consReason) != 1 {
		t.Errorf("byConsumerLastFailureReason: got %v, want {timeout:2}", consReason)
	}
}

func TestGetRdpStatus_Mixed(t *testing.T) {
	bindings := []any{
		row(true, ""),
		row(true, "old scar"), // recovered — stale reason must NOT bucket
		row(false, "boom"),
		row(false, ""), // down with no reason — counted, not bucketed
	}
	consumers := []any{
		row(false, "cert expired"),
		row(true, ""),
	}
	got, err := GetRdpStatus(steps(bindings, consumers))
	if err != nil {
		t.Fatal(err)
	}
	if got["bindingUpCount"] != 2 || got["bindingDownCount"] != 2 || got["bindingScannedCount"] != 4 {
		t.Errorf("binding counts wrong: %+v", got)
	}
	if got["consumerUpCount"] != 1 || got["consumerDownCount"] != 1 || got["consumerScannedCount"] != 2 {
		t.Errorf("consumer counts wrong: %+v", got)
	}
	bindReason := got["byBindingLastFailureReason"].(map[string]int)
	if len(bindReason) != 1 || bindReason["boom"] != 1 {
		t.Errorf("byBindingLastFailureReason should be {boom:1}, got %v", bindReason)
	}
	if _, present := bindReason["old scar"]; present {
		t.Errorf("recovered row's stale reason leaked into byBindingLastFailureReason: %v", bindReason)
	}
	if _, present := bindReason[""]; present {
		t.Errorf("empty-string bucket must be omitted, got %v", bindReason)
	}
	consReason := got["byConsumerLastFailureReason"].(map[string]int)
	if len(consReason) != 1 || consReason["cert expired"] != 1 {
		t.Errorf("byConsumerLastFailureReason should be {cert expired:1}, got %v", consReason)
	}
}

// TestGetRdpStatus_ActiveOnlyFilter is the load-bearing test for the
// by*LastFailureReason design choice: the maps reflect ACTIVE failures only.
// A row that is currently up MUST NOT contribute its (stale) lastFailureReason
// to the bucket, because SEMP retains the historical reason on recovered
// items — folding it in would report recovered scars as live failures.
func TestGetRdpStatus_ActiveOnlyFilter(t *testing.T) {
	bindings := []any{
		row(true, "yesterday's outage"), // recovered — MUST NOT bucket
		row(false, "today's outage"),    // active — bucketed
	}
	consumers := []any{
		row(true, "ancient history"), // recovered — MUST NOT bucket
	}
	got, err := GetRdpStatus(steps(bindings, consumers))
	if err != nil {
		t.Fatal(err)
	}
	bindReason := got["byBindingLastFailureReason"].(map[string]int)
	if len(bindReason) != 1 || bindReason["today's outage"] != 1 {
		t.Errorf("byBindingLastFailureReason should be {today's outage:1}, got %v", bindReason)
	}
	if _, present := bindReason["yesterday's outage"]; present {
		t.Errorf("recovered binding's stale reason leaked: %v", bindReason)
	}
	consReason := got["byConsumerLastFailureReason"].(map[string]int)
	if len(consReason) != 0 {
		t.Errorf("byConsumerLastFailureReason must be empty when only recovered consumers have reasons, got %v", consReason)
	}
}

// TestGetRdpStatus_MissingField asserts that omitting a required field on a
// row skips that row (mirrors listQueues / listRdps robustness). Iterates
// over each required field so reordering the handler's reads doesn't break
// the test for the wrong reason.
func TestGetRdpStatus_MissingField(t *testing.T) {
	full := row(false, "boom")
	for field := range full {
		t.Run(field, func(t *testing.T) {
			bad := map[string]any{}
			for k, v := range full {
				if k != field {
					bad[k] = v
				}
			}
			healthy := row(false, "other")
			got, err := GetRdpStatus(steps([]any{bad, healthy}, []any{healthy}))
			if err != nil {
				t.Fatalf("expected skip on missing %q, got %v", field, err)
			}
			if got["bindingSkipped"] != 1 {
				t.Errorf("bindingSkipped: got %v, want 1", got["bindingSkipped"])
			}
			if got["bindingScannedCount"] != 2 {
				t.Errorf("bindingScannedCount: got %v, want 2", got["bindingScannedCount"])
			}
			// Healthy row's signals must survive the skip.
			if got["bindingDownCount"] != 1 {
				t.Errorf("bindingDownCount: got %v, want 1", got["bindingDownCount"])
			}
			bindReason := got["byBindingLastFailureReason"].(map[string]int)
			if bindReason["other"] != 1 {
				t.Errorf("healthy row failed to bucket: %v", bindReason)
			}
		})
	}
}

// TestGetRdpStatus_WrongTypeField pins the handler's tolerance to a row where
// a required field is present but the wrong type (e.g. up delivered as a
// string). Must skip, not abort.
func TestGetRdpStatus_WrongTypeField(t *testing.T) {
	bad := map[string]any{"up": "true", "lastFailureReason": "boom"} // string, not bool
	healthy := row(false, "other")
	got, err := GetRdpStatus(steps([]any{bad, healthy}, []any{}))
	if err != nil {
		t.Fatalf("wrong-type field should skip, got %v", err)
	}
	if got["bindingSkipped"] != 1 {
		t.Errorf("bindingSkipped: got %v, want 1", got["bindingSkipped"])
	}
	if got["bindingDownCount"] != 1 {
		t.Errorf("healthy row's downCount lost: %+v", got)
	}
}

func TestGetRdpStatus_TruncationSurfaced(t *testing.T) {
	bindings := []any{row(true, ""), row(false, "x")}
	consumers := []any{row(true, "")}
	got, err := GetRdpStatus(map[string]map[string]any{
		rdpStatusBindingsStepID:  {"data": bindings, "truncated": true},
		rdpStatusConsumersStepID: {"data": consumers, "truncated": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["bindingTruncated"] != true {
		t.Errorf("bindingTruncated: got %v, want true", got["bindingTruncated"])
	}
	if _, present := got["consumerTruncated"]; present {
		t.Errorf("consumerTruncated should be omitted when false, got %v", got["consumerTruncated"])
	}
}

func TestGetRdpStatus_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing bindings step",
			input:   map[string]map[string]any{rdpStatusConsumersStepID: {"data": []any{}}},
			wantSub: `step "queueBindings" not in results`,
		},
		{
			name:    "missing consumers step",
			input:   map[string]map[string]any{rdpStatusBindingsStepID: {"data": []any{}}},
			wantSub: `step "restConsumers" not in results`,
		},
		{
			name: "bindings data wrong type",
			input: map[string]map[string]any{
				rdpStatusBindingsStepID:  {"data": "oops"},
				rdpStatusConsumersStepID: {"data": []any{}},
			},
			wantSub: "queueBindings.data: want []any",
		},
		{
			name: "consumers item wrong type",
			input: map[string]map[string]any{
				rdpStatusBindingsStepID:  {"data": []any{}},
				rdpStatusConsumersStepID: {"data": []any{"not-an-object"}},
			},
			wantSub: "restConsumers.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetRdpStatus(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestGetRdpStatus_ValidateTool exercises the boot-time cross-check for the
// getRdpStatus handler specifically. As the first multi-step handler, it
// verifies (a) each step's select is checked against that step's own fields,
// not the union — a field selected on the sibling step must still fail; and
// (b) declaring both step IDs in RequiredSteps guards against YAML renames.
func TestGetRdpStatus_ValidateTool(t *testing.T) {
	fullSelect := map[string][]string{
		rdpStatusBindingsStepID:  {"up", "lastFailureReason"},
		rdpStatusConsumersStepID: {"up", "lastFailureReason"},
	}

	t.Run("both steps cover their required fields", func(t *testing.T) {
		err := postprocess.ValidateTool("get-rdp-status", "getRdpStatus",
			[]string{rdpStatusBindingsStepID, rdpStatusConsumersStepID}, fullSelect)
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	t.Run("bindings select missing lastFailureReason", func(t *testing.T) {
		bad := map[string][]string{
			rdpStatusBindingsStepID:  {"up"}, // lastFailureReason dropped
			rdpStatusConsumersStepID: {"up", "lastFailureReason"},
		}
		err := postprocess.ValidateTool("get-rdp-status", "getRdpStatus",
			[]string{rdpStatusBindingsStepID, rdpStatusConsumersStepID}, bad)
		if err == nil || !strings.Contains(err.Error(), `"lastFailureReason" on step "queueBindings"`) {
			t.Fatalf("expected error naming lastFailureReason on queueBindings, got %v", err)
		}
	})

	t.Run("sibling select does not cover other step", func(t *testing.T) {
		// consumers has both fields; bindings has neither. The union check
		// would have passed — the per-step check must reject.
		bad := map[string][]string{
			rdpStatusBindingsStepID:  {},
			rdpStatusConsumersStepID: {"up", "lastFailureReason"},
		}
		err := postprocess.ValidateTool("get-rdp-status", "getRdpStatus",
			[]string{rdpStatusBindingsStepID, rdpStatusConsumersStepID}, bad)
		if err == nil || !strings.Contains(err.Error(), `on step "queueBindings"`) {
			t.Fatalf("per-step check must fail even when sibling covers the field, got %v", err)
		}
	})

	t.Run("step id renamed", func(t *testing.T) {
		err := postprocess.ValidateTool("get-rdp-status", "getRdpStatus",
			[]string{"bindings", "consumers"},
			map[string][]string{
				"bindings":  {"up", "lastFailureReason"},
				"consumers": {"up", "lastFailureReason"},
			})
		if err == nil || !strings.Contains(err.Error(), `"queueBindings"`) {
			t.Fatalf("expected error naming queueBindings step, got %v", err)
		}
	})
}
