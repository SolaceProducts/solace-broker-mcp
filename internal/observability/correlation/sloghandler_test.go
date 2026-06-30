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

package correlation

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// newJSONLogger returns a slog.Logger whose output is captured in buf, with the
// correlation wrapper installed over a plain JSON handler. It is the baseline
// fixture: no ReplaceAttr, just the wrapper over JSON, so tests can assert how
// the correlation_id attribute lands in the emitted record.
func newJSONLogger(buf *bytes.Buffer) *slog.Logger {
	json := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(NewSlogHandler(json))
}

// newRedactingLogger mirrors cmd/server/main.go newSlogHandler: a JSON handler
// carrying the SAME credential-redaction ReplaceAttr, wrapped by the correlation
// handler. It proves the wrapper delegates through ReplaceAttr (AC #2).
func newRedactingLogger(buf *bytes.Buffer) *slog.Logger {
	redactedKeys := []string{"password", "token", "secret", "authorization", "credential", "api_key", "private_key"}
	json := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			key := strings.ToLower(a.Key)
			for _, redacted := range redactedKeys {
				if strings.Contains(key, redacted) {
					a.Value = slog.StringValue("[REDACTED]")
					return a
				}
			}
			return a
		},
	})
	return slog.New(NewSlogHandler(json))
}

// parseLine decodes the single JSON log line captured in buf.
func parseLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatalf("no log output captured")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("expected exactly one log line, got:\n%s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

// AC #1: a request-scoped context carrying a correlation ID stamps
// correlation_id onto the emitted record.
func TestSlogHandler_AddsCorrelationIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf)

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hello")

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q", got, "abc")
	}
}

// AC #4 / transitive-off: a context with no correlation ID (the steady state
// when the middleware is not wired, or for startup/non-request logs) emits no
// correlation_id key, so output matches today exactly.
func TestSlogHandler_NoIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf)

	logger.InfoContext(context.Background(), "hello")

	m := parseLine(t, &buf)
	if _, present := m["correlation_id"]; present {
		t.Errorf("correlation_id unexpectedly present: %v", m)
	}
}

// AC #4: an empty-string ID on the context is treated as absent (From returns
// "" both when unset and when explicitly empty).
func TestSlogHandler_EmptyIDNotEmitted(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf)

	ctx := With(context.Background(), "")
	logger.InfoContext(ctx, "hello")

	m := parseLine(t, &buf)
	if _, present := m["correlation_id"]; present {
		t.Errorf("correlation_id unexpectedly present for empty id: %v", m)
	}
}

// AC #3: a record that already carries a correlation_id attribute is NOT
// overwritten, and no duplicate is added. We log an explicit correlation_id that
// differs from the context value and assert the explicit value survives.
func TestSlogHandler_DoesNotOverwriteExisting(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf)

	ctx := With(context.Background(), "from-ctx")
	logger.InfoContext(ctx, "hello", slog.String("correlation_id", "explicit"))

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "explicit" {
		t.Errorf("correlation_id = %v, want %q (existing attr must win)", got, "explicit")
	}

	// Assert no duplicate key was emitted: a duplicated attribute would show as
	// two occurrences in the raw line.
	if n := strings.Count(buf.String(), `"correlation_id"`); n != 1 {
		t.Errorf("expected exactly one correlation_id occurrence, got %d in: %s", n, buf.String())
	}
}

// AC #3: a correlation_id bound on the LOGGER at the root via .With(...) (held in
// goas, not on the record) must also win over the context ID — the wrapper must
// not inject a second root correlation_id. Without the goas root-level check, the
// context ID would be added at the root alongside the explicit one, producing a
// duplicate root key.
func TestSlogHandler_DoesNotDuplicateWithBoundRootCorrelationID(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).With(slog.String("correlation_id", "explicit"))

	ctx := With(context.Background(), "from-ctx")
	logger.InfoContext(ctx, "hello")

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "explicit" {
		t.Errorf("correlation_id = %v, want %q (With-bound root value must win)", got, "explicit")
	}
	// Exactly one root correlation_id: the context ID must not be injected.
	if n := strings.Count(buf.String(), `"correlation_id"`); n != 1 {
		t.Errorf("expected exactly one correlation_id occurrence, got %d in: %s", n, buf.String())
	}
}

// AC #3 boundary: a correlation_id bound via .With(...) AFTER a WithGroup nests
// under that group (a different key path), so it does NOT collide with the root.
// The wrapper must still inject the context ID at the root: the result has the
// context ID at root AND the explicit value nested under the group, no duplicate
// at any single level.
func TestSlogHandler_InjectsRootWhenWithBoundCorrelationIDIsNested(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).WithGroup("g").With(slog.String("correlation_id", "nested"))

	ctx := With(context.Background(), "from-ctx")
	logger.InfoContext(ctx, "hello")

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "from-ctx" {
		t.Errorf("root correlation_id = %v, want %q (context ID injected at root)", got, "from-ctx")
	}
	g, ok := m["g"].(map[string]any)
	if !ok {
		t.Fatalf("group g missing or not an object: %v", m)
	}
	if got := g["correlation_id"]; got != "nested" {
		t.Errorf("g.correlation_id = %v, want %q (nested explicit value preserved)", got, "nested")
	}
}

// AC #2: redaction is preserved because the wrapper delegates to the inner JSON
// handler that owns ReplaceAttr. A password attr is redacted, and the
// correlation_id added by the wrapper passes through ReplaceAttr intact (it is
// not a credential key).
func TestSlogHandler_PreservesRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := newRedactingLogger(&buf)

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hello", slog.String("password", "hunter2"))

	m := parseLine(t, &buf)
	if got := m["password"]; got != "[REDACTED]" {
		t.Errorf("password = %v, want %q", got, "[REDACTED]")
	}
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q", got, "abc")
	}
}

// WithAttrs must re-wrap so correlation behavior survives .With(...). After
// adding a base attr, a log on a correlation-bearing context still gets
// correlation_id.
func TestSlogHandler_WithAttrsPreservesCorrelation(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).With(slog.String("component", "test"))

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hello")

	m := parseLine(t, &buf)
	if got := m["component"]; got != "test" {
		t.Errorf("component = %v, want %q", got, "test")
	}
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q", got, "abc")
	}
}

// WithGroup must re-wrap so correlation behavior survives .WithGroup(...). The
// correlation_id is added at the record's top level (not inside the group),
// because rec.Add on the wrapped record is independent of the group set on the
// inner handler.
func TestSlogHandler_WithGroupPreservesCorrelation(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).WithGroup("g")

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hello", slog.String("k", "v"))

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q (must survive WithGroup)", got, "abc")
	}
}

// Under an open group, the record's own attributes nest inside the group while
// correlation_id stays at the root (a top-level field an operator can grep). The
// raw line is inspected because parseLine flattens to a map and the nesting
// matters here.
func TestSlogHandler_WithGroupKeepsCorrelationAtRoot(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).WithGroup("g")

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hello", slog.String("k", "v"))

	line := buf.String()
	if !strings.Contains(line, `"correlation_id":"abc"`) {
		t.Errorf("correlation_id not at root: %s", line)
	}
	if !strings.Contains(line, `"g":{"k":"v"}`) {
		t.Errorf("record attr not nested under group: %s", line)
	}
}

// Interleaved With/WithGroup/With: pre-group attrs and correlation_id land at
// the root; post-group attrs and the record's own attrs nest under the group.
// This locks the goa replay ordering.
func TestSlogHandler_MixedWithAndGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).
		With(slog.String("base", "b")).
		WithGroup("g").
		With(slog.String("ingroup", "x"))

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hi", slog.String("rec", "r"))

	m := parseLine(t, &buf)
	// correlation_id and base land at the root; ingroup and rec nest under g.
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q (root level)", got, "abc")
	}
	if got := m["base"]; got != "b" {
		t.Errorf("base = %v, want %q (root level)", got, "b")
	}
	g, ok := m["g"].(map[string]any)
	if !ok {
		t.Fatalf("group g missing or not an object: %v", m)
	}
	if got := g["ingroup"]; got != "x" {
		t.Errorf("g.ingroup = %v, want %q", got, "x")
	}
	if got := g["rec"]; got != "r" {
		t.Errorf("g.rec = %v, want %q", got, "r")
	}
}

// Nested groups: correlation_id stays at the root while the record's own attrs
// nest under the full group path. Locks the replay loop against future refactors.
func TestSlogHandler_NestedGroups(t *testing.T) {
	var buf bytes.Buffer
	logger := newJSONLogger(&buf).WithGroup("a").WithGroup("b")

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "hi", slog.String("rec", "r"))

	m := parseLine(t, &buf)
	if got := m["correlation_id"]; got != "abc" {
		t.Errorf("correlation_id = %v, want %q (root level)", got, "abc")
	}
	a, ok := m["a"].(map[string]any)
	if !ok {
		t.Fatalf("group a missing or not an object: %v", m)
	}
	b, ok := a["b"].(map[string]any)
	if !ok {
		t.Fatalf("group a.b missing or not an object: %v", m)
	}
	if got := b["rec"]; got != "r" {
		t.Errorf("a.b.rec = %v, want %q", got, "r")
	}
}

// Enabled delegates to the inner handler: a level below the handler threshold is
// dropped, proving the wrapper does not break level filtering.
func TestSlogHandler_EnabledDelegates(t *testing.T) {
	var buf bytes.Buffer
	json := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(NewSlogHandler(json))

	ctx := With(context.Background(), "abc")
	logger.InfoContext(ctx, "below threshold")

	if buf.Len() != 0 {
		t.Errorf("expected no output for sub-threshold level, got: %s", buf.String())
	}
}
