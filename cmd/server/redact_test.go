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
		logger := slog.New(newSlogHandler(slog.LevelInfo, nil))
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

// TestNewSlogHandler_IdentityAttrs pins the identityAttrs parameter
// (SOL-152425, Story 34): every line carries them when non-nil, and none do
// when nil (the two bootstrap/health-probe call sites, which run before cfg
// — and so the identity resource — exists).
func TestNewSlogHandler_IdentityAttrs(t *testing.T) {
	identity := []slog.Attr{
		slog.String("service.name", "test-service"),
		slog.String("deployment.environment.name", "test-env"),
	}

	buf := captureStderr(t, func() {
		logger := slog.New(newSlogHandler(slog.LevelInfo, identity))
		logger.Info("msg")
	})
	if !strings.Contains(buf, `"service.name":"test-service"`) {
		t.Fatalf("expected service.name in output, got:\n%s", buf)
	}
	if !strings.Contains(buf, `"deployment.environment.name":"test-env"`) {
		t.Fatalf("expected deployment.environment.name in output, got:\n%s", buf)
	}

	bufNil := captureStderr(t, func() {
		logger := slog.New(newSlogHandler(slog.LevelInfo, nil))
		logger.Info("msg")
	})
	if strings.Contains(bufNil, "service.name") {
		t.Fatalf("nil identityAttrs must add nothing, got:\n%s", bufNil)
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
