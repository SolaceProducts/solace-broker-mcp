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

package queuemetrics

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"testing"
)

// TestQueueDetailResponse_RoundTrip decodes the live show-queue-detail fixture
// into queueDetailResponse, marshals the resulting queueInfo to JSON, and
// asserts every curated live-state field round-trips with its fixture value.
// SOL-150785 (B): this is the one SEMPv1 read call in the codebase that had no
// dedicated response_test.go — only handler_test.go exercised it, and even
// that missed deliveredUnackedMsgCount/highWaterMarkBytes, so a regression in
// either field's xml tag would have gone undetected.
func TestQueueDetailResponse_RoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/show_queue_detail.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	var resp queueDetailResponse
	if err := xml.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("expected non-nil Info")
	}

	b, err := json.Marshal(resp.Info)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	checks := map[string]float64{
		"currentMsgCount":          10,
		"currentSpoolUsageBytes":   340,
		"deliveredUnackedMsgCount": 0,
		"highWaterMarkBytes":       340,
		"oldestMsgId":              12794319,
		"newestMsgId":              12794328,
	}
	for field, want := range checks {
		got, ok := out[field]
		if !ok {
			t.Errorf("%s: missing from JSON output", field)
			continue
		}
		gotFloat, ok := got.(float64)
		if !ok {
			t.Errorf("%s: not a float64: %T", field, got)
			continue
		}
		if gotFloat != want {
			t.Errorf("%s = %v, want %v", field, gotFloat, want)
		}
	}
	if len(out) != len(checks) {
		t.Errorf("unexpected field count: got %d fields %v, want %d", len(out), out, len(checks))
	}
}

// TestQueueDetailResponse_OmitsAbsentFields asserts oldestMsgId/newestMsgId
// (only meaningful when the queue holds messages) are omitted from the JSON
// output entirely, rather than surfacing as a misleading zero, when the
// broker's <info> block doesn't include them.
func TestQueueDetailResponse_OmitsAbsentFields(t *testing.T) {
	xmlDoc := `<show>
  <queue>
    <queues>
      <queue>
        <name>empty-queue</name>
        <info>
          <num-messages-spooled>0</num-messages-spooled>
          <current-spool-usage-in-bytes>0</current-spool-usage-in-bytes>
          <total-delivered-unacked-msgs>0</total-delivered-unacked-msgs>
          <high-water-mark-in-bytes>0</high-water-mark-in-bytes>
        </info>
      </queue>
    </queues>
  </queue>
</show>`

	var resp queueDetailResponse
	if err := xml.Unmarshal([]byte(xmlDoc), &resp); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("expected non-nil Info")
	}

	b, err := json.Marshal(resp.Info)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	for _, absent := range []string{"oldestMsgId", "newestMsgId"} {
		if _, present := out[absent]; present {
			t.Errorf("%s should be omitted when absent from the broker response, got %v", absent, out[absent])
		}
	}
	for _, present := range []string{"currentMsgCount", "currentSpoolUsageBytes", "deliveredUnackedMsgCount", "highWaterMarkBytes"} {
		if _, ok := out[present]; !ok {
			t.Errorf("%s should be present (broker sent it as 0), got omitted", present)
		}
	}
}
