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

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRedactSecretAttr covers the ReplaceAttr filter used by newSlogHandler.
// Matching is case-insensitive and substring-based, so a value under any key
// that contains one of the redactedKeys (in any casing) becomes [REDACTED],
// while keys that don't match pass through unchanged. SOL-150757.
func TestRedactSecretAttr(t *testing.T) {
	const sentinel = "SENTINEL_SECRET_VALUE"

	cases := []struct {
		name      string
		key       string
		wantRedac bool
	}{
		// Exact key matches for every entry in redactedKeys.
		{"password", "password", true},
		{"token", "token", true},
		{"secret", "secret", true},
		{"authorization", "authorization", true},
		{"credential", "credential", true},
		{"api_key", "api_key", true},
		{"private_key", "private_key", true},

		// Case-insensitivity.
		{"upper_PASSWORD", "PASSWORD", true},
		{"mixed_Api_Key", "Api_Key", true},

		// Substring match, not full-string match.
		{"substring_db_password", "db_password", true},
		{"substring_x_authorization_header", "x-authorization-header", true},
		{"substring_access_token_expiry", "access_token_expiry", true},

		// Non-matching keys pass through.
		{"passthrough_username", "username", false},
		{"passthrough_url", "url", false},
		{"passthrough_mode", "mode", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactSecretAttr(nil, slog.String(tc.key, sentinel))
			got := out.Value.String()
			if tc.wantRedac {
				if got != "[REDACTED]" {
					t.Fatalf("key=%q: want [REDACTED], got %q", tc.key, got)
				}
				return
			}
			if got != sentinel {
				t.Fatalf("key=%q: want %q untouched, got %q", tc.key, sentinel, got)
			}
		})
	}
}

// TestNewSlogHandler_RedactsInRenderedOutput exercises the full handler path
// (not just the filter in isolation): a logger built on newSlogHandler must
// emit [REDACTED] for a credential-keyed attribute and never leak the sentinel
// value into the JSON output. SOL-150757.
func TestNewSlogHandler_RedactsInRenderedOutput(t *testing.T) {
	const sentinel = "SENTINEL_SECRET_VALUE"

	// Swap os.Stderr for a pipe: newSlogHandler hard-codes os.Stderr as its
	// sink, and we don't want to change the signature for a test. The swap is
	// scoped and restored via defer.
	buf := captureStderr(t, func() {
		logger := slog.New(newSlogHandler(slog.LevelInfo))
		logger.Info("msg", slog.String("password", sentinel), slog.String("username", "alice"))
	})

	if strings.Contains(buf, sentinel) {
		t.Fatalf("rendered output leaked sentinel:\n%s", buf)
	}
	if !strings.Contains(buf, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in output, got:\n%s", buf)
	}
	if !strings.Contains(buf, "alice") {
		t.Fatalf("non-credential attr should pass through, got:\n%s", buf)
	}

	// Sanity-check that the emitted line is still valid JSON.
	if !json.Valid([]byte(firstLine(buf))) {
		t.Fatalf("output is not valid JSON:\n%s", buf)
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn, then
// returns everything written. Restored via defer before returning so callers
// don't inherit a stderr pointing at a closed pipe for the rest of the test.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()
	_ = w.Close()

	var out bytes.Buffer
	_, _ = out.ReadFrom(r)
	_ = r.Close()
	return out.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestRedactSecretAttr_FormatsDurations covers the second branch of the
// ReplaceAttr filter: a duration-kinded value is rendered via
// time.Duration.String() instead of the raw int64 nanoseconds slog's JSON
// handler would emit. Matching is on the value's KIND, not the key name, so the
// key is irrelevant here. SOL-153363.
func TestRedactSecretAttr_FormatsDurations(t *testing.T) {
	cases := []struct {
		name string
		key  string
		d    time.Duration
		want string
	}{
		// The motivating case: 77496667ns is unreadable as an integer.
		{"sub_second", "exchange_elapsed", 77496667 * time.Nanosecond, "77.496667ms"},
		{"whole_seconds", "drain_delay", 10 * time.Second, "10s"},
		{"minutes", "duration", 90 * time.Second, "1m30s"},
		{"millis", "retry_after", 250 * time.Millisecond, "250ms"},
		{"micros", "waited", 4 * time.Microsecond, "4µs"},

		// Edge cases: neither is special-cased, both must still be strings.
		{"zero", "clamped_to", 0, "0s"},
		{"negative", "requested", -5 * time.Millisecond, "-5ms"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactSecretAttr(nil, slog.Duration(tc.key, tc.d))
			if out.Value.Kind() != slog.KindString {
				t.Fatalf("kind = %v, want String (a duration must not stay numeric)", out.Value.Kind())
			}
			if got := out.Value.String(); got != tc.want {
				t.Fatalf("key=%q: got %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestRedactSecretAttr_RedactionBeatsDurationFormatting pins the ORDER of the
// two branches. A duration-kinded attribute under a credential-shaped key must
// be redacted, never formatted — the secrets check returns early, so it can
// never be the second thing that happens. No such attribute exists in the tree
// today (none of the nine duration keys match redactedKeys), so this guards the
// invariant against a future duration attr named e.g. token_ttl rather than
// describing current behavior. SOL-153363.
func TestRedactSecretAttr_RedactionBeatsDurationFormatting(t *testing.T) {
	for _, key := range []string{"token_ttl", "secret_timeout", "password_age"} {
		t.Run(key, func(t *testing.T) {
			out := redactSecretAttr(nil, slog.Duration(key, 30*time.Second))
			if got := out.Value.String(); got != "[REDACTED]" {
				t.Fatalf("key=%q: got %q, want [REDACTED] — redaction must win over duration formatting", key, got)
			}
		})
	}
}

// TestNewSlogHandler_RendersDurationsInOutput exercises the full handler path
// and asserts on the rendered JSON, which is what an operator actually reads:
// the duration must be a JSON *string* ("77.496667ms"), not the number
// 77496667. It also covers the two placements ReplaceAttr is easy to get wrong
// — an attribute nested inside a group, and one bound on the logger via
// .With() — because if either were missed some durations would silently stay
// numeric. Verified empirically that slog's JSON handler calls ReplaceAttr for
// both. SOL-153363.
func TestNewSlogHandler_RendersDurationsInOutput(t *testing.T) {
	out := captureStderr(t, func() {
		logger := slog.New(newSlogHandler(slog.LevelInfo))
		logger.Info("top level", slog.Duration("exchange_elapsed", 77496667*time.Nanosecond))
		logger.Info("in a group", slog.Group("g", slog.Duration("nested", 1500*time.Millisecond)))
		logger.With(slog.Duration("bound", 250*time.Millisecond)).Info("bound on logger")
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 log lines, got %d:\n%s", len(lines), out)
	}

	// Line 1: a top-level duration attr.
	var top struct {
		Elapsed any `json:"exchange_elapsed"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &top); err != nil {
		t.Fatalf("line 1 is not valid JSON: %v\n%s", err, lines[0])
	}
	if got, ok := top.Elapsed.(string); !ok || got != "77.496667ms" {
		t.Fatalf("exchange_elapsed = %#v, want string %q (raw nanoseconds decode as float64)", top.Elapsed, "77.496667ms")
	}

	// Line 2: nested under a group — ReplaceAttr fires for group CONTENTS.
	var grouped struct {
		G struct {
			Nested any `json:"nested"`
		} `json:"g"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &grouped); err != nil {
		t.Fatalf("line 2 is not valid JSON: %v\n%s", err, lines[1])
	}
	if got, ok := grouped.G.Nested.(string); !ok || got != "1.5s" {
		t.Fatalf("g.nested = %#v, want string %q", grouped.G.Nested, "1.5s")
	}

	// Line 3: bound via .With(), which slog pre-formats through ReplaceAttr.
	var bound struct {
		Bound any `json:"bound"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &bound); err != nil {
		t.Fatalf("line 3 is not valid JSON: %v\n%s", err, lines[2])
	}
	if got, ok := bound.Bound.(string); !ok || got != "250ms" {
		t.Fatalf("bound = %#v, want string %q", bound.Bound, "250ms")
	}
}
