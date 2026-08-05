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
)

// kafkaReceiver builds a Kafka Receiver item with the three required fields.
func kafkaReceiver(up, enabled bool, reason string) map[string]any {
	return map[string]any{
		"up":            up,
		"enabled":       enabled,
		"failureReason": reason,
	}
}

func TestListKafkaReceivers_Counts(t *testing.T) {
	items := []any{
		kafkaReceiver(true, true, ""),                          // healthy
		kafkaReceiver(false, true, "connection refused"),       // down (enabled && !up) + bucketed
		kafkaReceiver(false, false, "Kafka Receiver Shutdown"), // disabled only — NOT down (admin-disabled excluded), NOT bucketed
		kafkaReceiver(true, false, ""),                         // disabled only
		kafkaReceiver(false, true, "connection refused"),       // down (enabled && !up) + same bucket
		kafkaReceiver(true, true, "connection refused"),        // recovered, stale reason — must NOT bucket
	}
	got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"downCount":     2,
		"disabledCount": 2,
		"scanned":       6,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
	byReason, ok := got["byFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byFailureReason: wrong type %T", got["byFailureReason"])
	}
	// If bucketing weren't gated on !up, the recovered row above would push
	// this to 3.
	if byReason["connection refused"] != 2 || len(byReason) != 1 {
		t.Errorf("byFailureReason: got %v, want {connection refused:2}", byReason)
	}
	if _, present := byReason["Kafka Receiver Shutdown"]; present {
		t.Errorf("admin-disabled receiver's reason must not bucket, got %v", byReason)
	}
}

// TestListKafkaReceivers_SkipsMalformedRowAndSurfacesTruncation covers the
// two branches TestListKafkaReceivers_Counts doesn't reach: a row missing a
// required field (skipped rather than aborting the call) and the truncated
// flag passing through. ListKafkaReceivers is a hand-mirrored copy of
// ListKafkaSenders/ListRdps, not a shared helper — each copy's own skip and
// truncation branches need their own test, or a bug introduced in just this
// copy goes undetected.
func TestListKafkaReceivers_SkipsMalformedRowAndSurfacesTruncation(t *testing.T) {
	items := []any{
		kafkaReceiver(false, true, "boom"), // healthy-shaped: down + bucketed
		map[string]any{"up": true},         // missing enabled/failureReason → skipped
	}
	got, err := ListKafkaReceivers(map[string]map[string]any{
		"kafkaReceivers": {"data": items, "truncated": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", got["skipped"])
	}
	if got["downCount"] != 1 {
		t.Errorf("downCount: got %v, want 1 (skipped row must not count)", got["downCount"])
	}
	if got["truncated"] != true {
		t.Errorf("truncated: got %v, want true", got["truncated"])
	}
}

func TestListKafkaReceivers_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "kafkaReceivers" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"kafkaReceivers": {"data": "oops"}},
			wantSub: "kafkaReceivers.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"kafkaReceivers": {"data": []any{"not-an-object"}}},
			wantSub: "kafkaReceivers.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListKafkaReceivers(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
