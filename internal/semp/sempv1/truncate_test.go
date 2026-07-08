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

package sempv1

import (
	"strings"
	"testing"
)

// TestTruncateErrorText covers the helper directly: short input is returned
// unchanged; oversized input is cut to the cap plus marker and stays valid
// UTF-8 (no rune split).
func TestTruncateErrorText(t *testing.T) {
	t.Run("short input unchanged", func(t *testing.T) {
		in := "a normal broker error"
		if got := truncateErrorText(in); got != in {
			t.Errorf("short input changed: got %q want %q", got, in)
		}
	})

	t.Run("oversized input truncated with marker", func(t *testing.T) {
		in := strings.Repeat("A", maxErrorTextLen*4)
		got := truncateErrorText(in)
		if len(got) > maxErrorTextLen+len(truncationMarker) {
			t.Errorf("not truncated: len=%d, cap=%d", len(got), maxErrorTextLen+len(truncationMarker))
		}
		if !strings.HasSuffix(got, truncationMarker) {
			t.Errorf("missing truncation marker; tail=%q", got[len(got)-len(truncationMarker):])
		}
	})

	t.Run("multibyte input stays valid UTF-8", func(t *testing.T) {
		// '€' is 3 bytes; a run of them crosses the cap mid-rune, exercising
		// the rune-boundary backup.
		got := truncateErrorText(strings.Repeat("€", maxErrorTextLen))
		if !strings.HasSuffix(got, truncationMarker) {
			t.Fatalf("expected truncation, got tail %q", got[len(got)-len(truncationMarker):])
		}
		body := strings.TrimSuffix(got, truncationMarker)
		if !strings.ContainsRune(body, '€') || strings.ContainsRune(body, '�') {
			t.Errorf("truncation split a rune: %q", body[len(body)-6:])
		}
	})
}

// TestParseReply_TruncatesOversizedMessage proves the capture-site fix: a
// broker <parse-error> larger than the cap yields a bounded Error.Message,
// so Error(), the tool log detail, and the agent message are all bounded.
func TestParseReply_TruncatesOversizedMessage(t *testing.T) {
	huge := strings.Repeat("A", maxErrorTextLen*4)
	body := "<rpc-reply><parse-error>" + huge + "</parse-error></rpc-reply>"

	_, err := parseReply([]byte(body))
	if err == nil {
		t.Fatal("expected an error, got nil")
		return
	}
	if err.Kind != ErrorKindParse {
		t.Fatalf("kind mismatch: got %v want %v", err.Kind, ErrorKindParse)
	}
	if len(err.Message) > maxErrorTextLen+len(truncationMarker) {
		t.Errorf("Message not truncated: len=%d", len(err.Message))
	}
	if !strings.HasSuffix(err.Message, truncationMarker) {
		t.Errorf("expected truncation marker on Message")
	}
}
