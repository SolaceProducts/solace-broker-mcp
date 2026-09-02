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

// Golden tests for the "tool invoked" audit line and the destructive-
// operation WARN (SOL-152087, step 0).
//
// The SOL-149606 tests in manager_test.go assert identity fields by key,
// which pins presence but not shape: key order, the exact set of keys, and
// the byte rendering of each value are unchecked. Story 20 re-sources
// tools.Identity from auth.Principal and requires the emitted line to be
// byte-identical before and after, so these goldens capture the line as it
// is TODAY, before the refactor, and CI enforces the requirement instead of
// whoever remembers it.
//
// The two nondeterministic attributes — the record timestamp and the
// measured duration — are pinned to fixed values by ReplaceAttr so the rest
// of the line can be compared as a string. Everything else is rendered
// exactly as production would render it through slog.NewJSONHandler.
//
// Intentionally brittle: a diff here is a change to the audit-log contract
// and must be a deliberate, reviewed decision (Story 22 will make one).

package tools

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// goldenDuration is the fixed value substituted for the measured duration.
const goldenDuration = 1500 * time.Microsecond

// captureGoldenLogs runs fn with a JSON slog handler installed as the default
// logger whose time and duration attributes are pinned, and returns the raw
// emitted audit lines in order. Only the two records under test are kept —
// "tool invoked" and the destructive-operation WARN — so unrelated INFO
// lines emitted on the same path (the pool's "broker connection created" on
// first resolution of an alias) do not perturb the golden.
func captureGoldenLogs(t *testing.T, level slog.Level, fn func()) []string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) != 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.String(slog.TimeKey, "T")
			case "duration":
				return slog.Duration("duration", goldenDuration)
			}
			return a
		},
	})))
	defer slog.SetDefault(old)

	fn()

	var out []string
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.Contains(line, `"msg":"tool invoked"`) ||
			strings.Contains(line, `"msg":"executing destructive operation"`) {
			out = append(out, line)
		}
	}
	return out
}

func TestToolInvokedLine_golden(t *testing.T) {
	const identity = `"sub":"auth0|abc123","iss":"https://example.auth0.com/","client_id":"cursor-ide","jti":"jti-xyz"`

	cases := []struct {
		name  string
		level slog.Level
		id    Identity
		call  func(mgr *ToolManager, id Identity)
		want  []string
	}{
		{
			name:  "success, present-mode identity",
			level: slog.LevelInfo,
			id:    idFixture(),
			call: func(mgr *ToolManager, id Identity) {
				if _, err := mgr.CallTool(context.Background(), "test-tool",
					map[string]any{"broker": "dev", "msgVpnName": "default"}, id); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			},
			want: []string{
				`{"time":"T","level":"INFO","msg":"tool invoked","tool":"test-tool","broker":"dev","status":"success","duration":1500000,` + identity + `}`,
			},
		},
		{
			name:  "error, present-mode identity",
			level: slog.LevelError,
			id:    idFixture(),
			call: func(mgr *ToolManager, id Identity) {
				// Missing broker: the local-error branch, brokerless, so the
				// line carries no broker attr and detail is the Go type only.
				_, _ = mgr.CallTool(context.Background(), "test-tool",
					map[string]any{"msgVpnName": "default"}, id)
			},
			want: []string{
				`{"time":"T","level":"ERROR","msg":"tool invoked","tool":"test-tool","status":"error","error_type":"missing_broker","duration":1500000,"detail":"*errors.errorString",` + identity + `}`,
			},
		},
		{
			name:  "destructive WARN then success, present-mode identity",
			level: slog.LevelWarn,
			id:    idFixture(),
			call: func(mgr *ToolManager, id Identity) {
				if _, err := mgr.CallTool(context.Background(), "destructive-tool",
					map[string]any{"broker": "dev", "msgVpnName": "default"}, id); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			},
			want: []string{
				`{"time":"T","level":"WARN","msg":"executing destructive operation","tool":"destructive-tool","broker":"dev",` + identity + `}`,
			},
		},
		{
			name:  "success, disabled mode (no identity keys)",
			level: slog.LevelInfo,
			id:    Identity{},
			call: func(mgr *ToolManager, id Identity) {
				if _, err := mgr.CallTool(context.Background(), "test-tool",
					map[string]any{"broker": "dev", "msgVpnName": "default"}, id); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			},
			want: []string{
				`{"time":"T","level":"INFO","msg":"tool invoked","tool":"test-tool","broker":"dev","status":"success","duration":1500000}`,
			},
		},
		{
			name:  "error, disabled mode (no identity keys)",
			level: slog.LevelError,
			id:    Identity{},
			call: func(mgr *ToolManager, id Identity) {
				_, _ = mgr.CallTool(context.Background(), "test-tool",
					map[string]any{"msgVpnName": "default"}, id)
			},
			want: []string{
				`{"time":"T","level":"ERROR","msg":"tool invoked","tool":"test-tool","status":"error","error_type":"missing_broker","duration":1500000,"detail":"*errors.errorString"}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewToolManager(newTestPool(t))
			mgr.Register(newStubHandler("test-tool"))
			destructive := newStubHandler("destructive-tool")
			isDestructive := true
			destructive.annotations = Annotations{Destructive: &isDestructive}
			mgr.Register(destructive)

			got := captureGoldenLogs(t, tc.level, func() { tc.call(mgr, tc.id) })

			if len(got) != len(tc.want) {
				t.Fatalf("emitted %d line(s), want %d:\n%s", len(got), len(tc.want), strings.Join(got, "\n"))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d differs from golden.\n got: %s\nwant: %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}
