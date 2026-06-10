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

package sempv2

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Broker-controlled error text is bounded at capture time so a misbehaving
// broker or intermediary returning a multi-MiB error body cannot blow up
// log records, MCP error results, or the agent's context. Capture-time
// truncation bounds every downstream sink at once (Error(), logToolResult,
// buildSEMPv2Message).

func TestParseSEMPError_TruncatesOversizedBody(t *testing.T) {
	// Unparseable body (HTML error page from a proxy) far over the cap.
	body := []byte("<html>" + strings.Repeat("x", 1024*1024) + "</html>")
	e := parseSEMPError("listQueues", 502, body)

	if len(e.Body) > maxErrorTextLen+len(truncationMarker) {
		t.Errorf("Body length = %d, want <= %d", len(e.Body), maxErrorTextLen+len(truncationMarker))
	}
	if !strings.HasSuffix(e.Body, truncationMarker) {
		t.Errorf("truncated Body should end with %q", truncationMarker)
	}
	if msg := e.Error(); len(msg) > maxErrorTextLen+256 {
		t.Errorf("Error() length = %d — raw-Body fallback not bounded", len(msg))
	}
}

func TestParseSEMPError_TruncatesOversizedDescription(t *testing.T) {
	desc := strings.Repeat("d", 1024*1024)
	body := fmt.Sprintf(`{"meta":{"error":{"code":11,"status":"FAIL","description":%q}}}`, desc)
	e := parseSEMPError("getMsgVpn", 400, []byte(body))

	if len(e.Description) > maxErrorTextLen+len(truncationMarker) {
		t.Errorf("Description length = %d, want <= %d", len(e.Description), maxErrorTextLen+len(truncationMarker))
	}
	if !strings.HasSuffix(e.Description, truncationMarker) {
		t.Errorf("truncated Description should end with %q", truncationMarker)
	}
	// Structured fields must still be extracted from a parseable envelope.
	if e.SEMPCode != 11 || e.SEMPStatus != "FAIL" {
		t.Errorf("structured fields lost: code=%d status=%q", e.SEMPCode, e.SEMPStatus)
	}
}

func TestParseSEMPError_SmallTextUnchanged(t *testing.T) {
	body := []byte(`{"meta":{"error":{"code":6,"status":"NOT_FOUND","description":"queue not found"}}}`)
	e := parseSEMPError("getQueue", 400, body)

	if e.Description != "queue not found" {
		t.Errorf("Description = %q, want unchanged", e.Description)
	}
	if e.Body != string(body) {
		t.Error("Body should be unchanged when under the cap")
	}
}

func TestTruncateErrorText_DoesNotSplitRune(t *testing.T) {
	// Fill so a 3-byte rune straddles the cut point.
	s := strings.Repeat("a", maxErrorTextLen-1) + "世界"
	got := truncateErrorText(s)

	if !utf8.ValidString(got) {
		t.Errorf("truncated text is not valid UTF-8")
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("expected truncation marker suffix")
	}
}
