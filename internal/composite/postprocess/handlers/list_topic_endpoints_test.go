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

// topicEndpoint builds a topic endpoint item with the four required fields.
// usageBytes is msgSpoolUsage in bytes; maxUsageMB is maxSpoolUsage in
// megabytes. Both units are from the MsgVpnTopicEndpoint monitor spec
// (10.8.1 and 10.26.8). Note the quota field is maxSpoolUsage, not the
// queue's maxMsgSpoolUsage.
func topicEndpoint(bindCount, usageBytes, maxUsageMB float64, state string) map[string]any {
	return map[string]any{
		"bindCount":                     bindCount,
		"msgSpoolUsage":                 usageBytes,
		"maxSpoolUsage":                 maxUsageMB,
		"lowPriorityMsgCongestionState": state,
	}
}

func TestListTopicEndpoints_Counts(t *testing.T) {
	items := []any{
		topicEndpoint(0, 0, 100, "not-congested"),             // noConsumer
		topicEndpoint(0, 90*bytesPerMB, 100, "congested"),     // noConsumer + congested + nearFull (0.90)
		topicEndpoint(1, 99*bytesPerMB, 100, "not-congested"), // nearFull (0.99)
		topicEndpoint(1, 50*bytesPerMB, 100, "not-congested"), // nothing (0.50)
		topicEndpoint(1, 0, 0, "not-congested"),               // unbounded: skip nearFull
	}
	got, err := ListTopicEndpoints(map[string]map[string]any{
		"topicEndpoints": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]int{
		"noConsumerCount": 2,
		"congestedCount":  1,
		"nearFullCount":   2,
		"scanned":         5,
	} {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
}

// The 80% boundary is inclusive, and the comparison is in bytes: a quota in MB
// read as bytes would flag almost every non-empty endpoint (SOL-150260 was the
// same bug on queues).
func TestListTopicEndpoints_NearFullBoundaryAndUnits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		usageBytes float64
		maxMB      float64
		want       int
	}{
		{"exactly 80% is near full", 80 * bytesPerMB, 100, 1},
		{"79.9% is not", 79.9 * bytesPerMB, 100, 0},
		{"0.8 MB of 100 MB is 0.8%, not near full", 0.8 * bytesPerMB, 100, 0},
		{"quota 0 is unbounded, skipped", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListTopicEndpoints(map[string]map[string]any{
				"topicEndpoints": {"data": []any{topicEndpoint(1, tc.usageBytes, tc.maxMB, "not-congested")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got["nearFullCount"] != tc.want {
				t.Errorf("nearFullCount: got %v, want %d", got["nearFullCount"], tc.want)
			}
		})
	}
}

// A row carrying the queue's quota field instead of maxSpoolUsage is a row
// missing its quota: it is skipped, not read as unbounded or as a queue.
func TestListTopicEndpoints_QueueQuotaFieldIsNotRead(t *testing.T) {
	row := topicEndpoint(1, 90*bytesPerMB, 100, "not-congested")
	delete(row, "maxSpoolUsage")
	row["maxMsgSpoolUsage"] = float64(100)
	got, err := ListTopicEndpoints(map[string]map[string]any{"topicEndpoints": {"data": []any{row}}})
	if err != nil {
		t.Fatal(err)
	}
	if got["skipped"] != 1 || got["nearFullCount"] != 0 {
		t.Errorf("got %+v, want skipped 1 and nearFullCount 0", got)
	}
}

func TestListTopicEndpoints_TruncationSurfaced(t *testing.T) {
	items := []any{
		topicEndpoint(0, 0, 100, "not-congested"),
		topicEndpoint(1, 50*bytesPerMB, 100, "not-congested"),
	}
	t.Run("truncated with cursor", func(t *testing.T) {
		got, err := ListTopicEndpoints(map[string]map[string]any{
			"topicEndpoints": {"data": items, "truncated": true, "nextCursor": "c1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got["truncated"] != true || got["scanned"] != 2 {
			t.Errorf("got %+v, want truncated true and scanned 2", got)
		}
		note, _ := got["scopeNote"].(string)
		if !strings.Contains(note, "2 topic endpoints") || !strings.Contains(note, "If topicEndpoints.nextCursor is present") {
			t.Errorf("scopeNote: got %q, want the 2 scanned endpoints and a pointer at topicEndpoints.nextCursor", note)
		}
	})
	t.Run("truncated without cursor", func(t *testing.T) {
		got, err := ListTopicEndpoints(map[string]map[string]any{
			"topicEndpoints": {"data": items, "truncated": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		note, _ := got["scopeNote"].(string)
		if !strings.Contains(note, "2 topic endpoints") || strings.Contains(note, "nextCursor") {
			t.Errorf("scopeNote: got %q, want the scan size and no nextCursor pointer", note)
		}
	})
	t.Run("not truncated", func(t *testing.T) {
		got, err := ListTopicEndpoints(map[string]map[string]any{
			"topicEndpoints": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"truncated", "scopeNote"} {
			if _, present := got[k]; present {
				t.Errorf("%s should be omitted, got %v", k, got[k])
			}
		}
	})
	t.Run("resumed last page", func(t *testing.T) {
		got, err := ListTopicEndpoints(map[string]map[string]any{
			"topicEndpoints": {"data": items, "resumed": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		note, _ := got["scopeNote"].(string)
		if !strings.Contains(note, "earlier pages") || strings.Contains(note, "nextCursor") {
			t.Errorf("scopeNote: got %q, want the earlier-pages caveat and no nextCursor pointer", note)
		}
	})
}

func TestListTopicEndpoints_Errors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{"missing step", map[string]map[string]any{}, `step "topicEndpoints" not in results`},
		{"data wrong type", map[string]map[string]any{"topicEndpoints": {"data": "oops"}}, "topicEndpoints.data: want []any"},
		{"item wrong type", map[string]map[string]any{"topicEndpoints": {"data": []any{"x"}}}, "topicEndpoints.data[0]: want object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListTopicEndpoints(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// One row with a missing, nil or wrong-typed field is skipped from every
// count; the other rows still count.
func TestListTopicEndpoints_BadFieldSkipsRow(t *testing.T) {
	full := topicEndpoint(0, 0, 100, "not-congested")
	for field := range full {
		for _, variant := range []struct {
			name string
			set  func(m map[string]any)
		}{
			{"missing", func(m map[string]any) { delete(m, field) }},
			{"nil", func(m map[string]any) { m[field] = nil }},
			{"wrong type", func(m map[string]any) { m[field] = []any{} }},
		} {
			t.Run(field+"/"+variant.name, func(t *testing.T) {
				bad := topicEndpoint(0, 0, 100, "not-congested")
				variant.set(bad)
				healthy := topicEndpoint(2, 90*bytesPerMB, 100, "congested")
				got, err := ListTopicEndpoints(map[string]map[string]any{"topicEndpoints": {"data": []any{bad, healthy}}})
				if err != nil {
					t.Fatalf("want a skip, got error %v", err)
				}
				if got["skipped"] != 1 || got["scanned"] != 2 {
					t.Errorf("got %+v, want skipped 1 and scanned 2", got)
				}
				for _, k := range []string{"congestedCount", "nearFullCount"} {
					if got[k] != 1 {
						t.Errorf("%s: got %v, want 1 from the healthy row", k, got[k])
					}
				}
				if got["noConsumerCount"] != 0 {
					t.Errorf("noConsumerCount: got %v, want 0 (the skipped row must not count)", got["noConsumerCount"])
				}
			})
		}
	}
}

func TestListTopicEndpoints_EmptyOmitsSkipped(t *testing.T) {
	got, err := ListTopicEndpoints(map[string]map[string]any{"topicEndpoints": {"data": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"noConsumerCount", "congestedCount", "nearFullCount", "scanned"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped should be omitted when 0, got %v", got["skipped"])
	}
}
