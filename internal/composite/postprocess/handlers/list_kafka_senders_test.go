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

// kafkaSender builds a Kafka Sender item with the three required fields.
func kafkaSender(up, enabled bool, reason string) map[string]any {
	return map[string]any{
		"up":            up,
		"enabled":       enabled,
		"failureReason": reason,
	}
}

func TestListKafkaSenders_Counts(t *testing.T) {
	items := []any{
		kafkaSender(true, true, ""),                        // healthy
		kafkaSender(false, true, "connection refused"),     // down (enabled && !up) + bucketed
		kafkaSender(false, false, "Kafka Sender Shutdown"), // disabled only — NOT down (admin-disabled excluded), NOT bucketed
		kafkaSender(true, false, ""),                       // disabled only
		kafkaSender(false, true, "connection refused"),     // down (enabled && !up) + same bucket
		kafkaSender(true, true, "connection refused"),      // recovered, stale reason — must NOT bucket
	}
	got, err := ListKafkaSenders(map[string]map[string]any{"kafkaSenders": {"data": items}})
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
	if _, present := byReason["Kafka Sender Shutdown"]; present {
		t.Errorf("admin-disabled sender's reason must not bucket, got %v", byReason)
	}
}

// TestListKafkaSenders_SkipsMalformedRowAndSurfacesTruncation covers the two
// branches TestListKafkaSenders_Counts doesn't reach: a row missing a
// required field (skipped rather than aborting the call) and the truncated
// flag passing through. ListKafkaSenders is a hand-mirrored copy of
// ListKafkaReceivers/ListRdps, not a shared helper — each copy's own skip and
// truncation branches need their own test, or a bug introduced in just this
// copy goes undetected.
func TestListKafkaSenders_SkipsMalformedRowAndSurfacesTruncation(t *testing.T) {
	items := []any{
		kafkaSender(false, true, "boom"), // healthy-shaped: down + bucketed
		map[string]any{"up": true},       // missing enabled/failureReason → skipped
	}
	got, err := ListKafkaSenders(map[string]map[string]any{
		"kafkaSenders": {"data": items, "truncated": true},
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

func TestListKafkaSenders_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "kafkaSenders" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"kafkaSenders": {"data": "oops"}},
			wantSub: "kafkaSenders.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"kafkaSenders": {"data": []any{"not-an-object"}}},
			wantSub: "kafkaSenders.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListKafkaSenders(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
