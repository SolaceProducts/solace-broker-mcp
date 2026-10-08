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

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests pin what the agent and the operator see when a composite write
// tool rejects a bad request body locally (SOL-155412). They run each tool's
// real catalog definition and real executor behind ToolManager.CallTool, so the
// reply text and the log line come from the real mapping and logging code, not
// from a hand-built error.

// requestBodyOutcome is what one call looks like to the agent (reply) and to
// the operator (log line).
type requestBodyOutcome struct {
	result *mcp.CallToolResult
	reply  string
	log    map[string]any
	client *mockClient
}

// callRealWriteTool drives toolName's real definition and executor through
// CallTool. Tests that use it must not run in parallel: it swaps the global
// slog default to capture the "tool invoked" line.
func callRealWriteTool(t *testing.T, toolName string, params map[string]any) requestBodyOutcome {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(old)

	real, client := realOwnerValidationFixture(t, toolName)
	mgr := NewToolManager(newTestPool(t))
	stub := newStubHandler(toolName)
	stub.schema = map[string]any{"type": "object"}
	stub.handleFn = func(ctx context.Context, _ *ToolContext, p map[string]any) (*ToolResult, error) {
		return real.Handle(ctx, &ToolContext{SEMPv2Client: client}, p)
	}
	mgr.Register(stub)

	args := map[string]any{"broker": "dev"}
	for k, v := range params {
		args[k] = v
	}
	result, err := mgr.CallTool(context.Background(), toolName, args, Identity{})
	return requestBodyOutcome{
		result: result,
		reply:  callToolResultText(t, result, err),
		log:    toolInvokedLine(t, &buf, toolName),
		client: client,
	}
}

// callStubError runs a stub handler that returns handlerErr, to check how the
// manager treats an error that is not one of the local request-body rejections.
func callStubError(t *testing.T, handlerErr error) (*mcp.CallToolResult, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool(t))
	stub := newStubHandler("update-queue")
	stub.schema = map[string]any{"type": "object"}
	stub.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handlerErr
	}
	mgr.Register(stub)

	result, err := mgr.CallTool(context.Background(), "update-queue", map[string]any{"broker": "dev"}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	return result, toolInvokedLine(t, &buf, "update-queue")
}

func toolInvokedLine(t *testing.T, buf *bytes.Buffer, tool string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line is not JSON: %v\nline: %s", err, line)
		}
		if fields["msg"] == "tool invoked" && fields["tool"] == tool {
			return fields
		}
	}
	t.Fatalf("no \"tool invoked\" log line for %q in:\n%s", tool, buf.String())
	return nil
}

// requireOwnMessage fails unless the agent got the executor's own message and
// nothing was sent to the broker.
func requireOwnMessage(t *testing.T, out requestBodyOutcome, want ...string) {
	t.Helper()
	if strings.Contains(out.reply, genericInternalMessage) {
		t.Errorf("agent got the generic broker-error text: %q", out.reply)
	}
	// The agent gets the executor's own sentence, not the chain of wrappers the
	// error picks up on its way up.
	if strings.Contains(out.reply, "executing tool") || strings.Contains(out.reply, "tool step") {
		t.Errorf("reply carries the internal wrapper prefix: %q", out.reply)
	}
	for _, w := range want {
		if !strings.Contains(out.reply, w) {
			t.Errorf("reply = %q, want it to contain %q", out.reply, w)
		}
	}
	if len(out.client.calls) != 0 {
		t.Errorf("broker client was called %v; a locally rejected body must not send a request", out.client.calls)
	}
}

// P1: an attribute the operation does not declare (the ticket's step 1: a
// queue-style spool field name on a topic endpoint).
func TestRequestBodyRejection_UnknownAttribute_AgentSeesOwnMessage(t *testing.T) {
	out := callRealWriteTool(t, "update-topic-endpoint", map[string]any{
		"msgVpnName":          "default",
		"topicEndpointName":   "te1",
		"topicEndpointConfig": map[string]any{"maxMsgSpoolUsage": 5},
	})
	requireOwnMessage(t, out,
		"is not a known attribute",
		`"maxMsgSpoolUsage"`,
		"updateMsgVpnTopicEndpoint")
}

// P2: a path parameter placed inside the config object (the ticket's step 3).
func TestRequestBodyRejection_PathParamInConfig_AgentSeesOwnMessage(t *testing.T) {
	out := callRealWriteTool(t, "update-queue", map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"msgVpnName": "default"},
	})
	requireOwnMessage(t, out,
		"must not appear in",
		`"queueConfig"`)
}

// P3: the same field supplied twice. create-queue takes queueName into the
// body, so naming it again inside queueConfig is ambiguous.
func TestRequestBodyRejection_DuplicateField_AgentSeesOwnMessage(t *testing.T) {
	out := callRealWriteTool(t, "create-queue", map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"queueName": "q2"},
	})
	requireOwnMessage(t, out,
		"defined more than once",
		`"queueName"`)
}

// The mapper returns exactly the executor's text for an error that arrives
// wrapped the way CallTool wraps it, with nothing added in front or behind. The
// error comes from the real executor, so this also fails if a site stops
// returning the dedicated type.
func TestBuildErrorMessage_RequestBodyRejection_IsTheExecutorsExactText(t *testing.T) {
	handler, client := realOwnerValidationFixture(t, "update-queue")
	_, err := handler.Handle(context.Background(), &ToolContext{SEMPv2Client: client}, map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"notARealAttr": 1},
	})
	if err == nil {
		t.Fatal("expected the executor to reject the request body")
	}

	var bodyErr *composite.RequestBodyError
	if !errors.As(err, &bodyErr) {
		t.Fatalf("executor error = %T, want a *composite.RequestBodyError in the chain", err)
	}
	want := bodyErr.Error()
	// The executor already adds "tool step <id>:" in front, so the full error text
	// is longer than the sentence the agent should get.
	if err.Error() == want {
		t.Fatalf("expected the executor to wrap the rejection, but err.Error() = %q", err.Error())
	}

	wrapped := fmt.Errorf("executing tool %q: %w", "update-queue", err)
	msg, suggestions := buildErrorMessage(wrapped, "dev")
	if msg != want {
		t.Errorf("message = %q, want exactly the executor's own text %q", msg, want)
	}
	if len(suggestions) != 0 {
		t.Errorf("suggestions = %v, want none for a local rejection", suggestions)
	}
	if text, ok := requestBodyErrorText(wrapped); !ok || text != want {
		t.Errorf("requestBodyErrorText = %q, %v; want %q, true", text, ok, want)
	}
}

// A hostile key name is echoed back to the agent only in its quoted, escaped
// form: no raw newline reaches the reply.
func TestRequestBodyRejection_HostileKeyIsEscapedInReply(t *testing.T) {
	out := callRealWriteTool(t, "update-queue", map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"bad\nkey\"x": 1},
	})
	requireOwnMessage(t, out, "is not a known attribute")
	if strings.Contains(out.reply, "\n") {
		t.Errorf("reply contains a raw newline from the caller's key: %q", out.reply)
	}
	if !strings.Contains(out.reply, `bad\nkey\"x`) {
		t.Errorf("reply = %q, want the key in escaped form", out.reply)
	}

	// The log carries the same escaped sentence, so a hostile key cannot add a
	// line or forge a field there either.
	detail, _ := out.log["detail"].(string)
	if detail != out.reply {
		t.Errorf("detail = %q, want exactly the reply %q", detail, out.reply)
	}
	if strings.Contains(detail, "\n") {
		t.Errorf("detail contains a raw newline from the caller's key: %q", detail)
	}
}

// A very long key is repeated only up to a bound, in the reply and in the ERROR
// log line, so one call cannot put kilobytes of caller text into default-level
// logs. The two channels still say exactly the same thing.
func TestRequestBodyRejection_VeryLongKeyIsBounded(t *testing.T) {
	longKey := strings.Repeat("k", 500)
	out := callRealWriteTool(t, "update-queue", map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{longKey: 1},
	})
	requireOwnMessage(t, out, "is not a known attribute")

	kept := strings.Repeat("k", 128) + "…"
	if !strings.Contains(out.reply, kept) {
		t.Errorf("reply = %q, want the key cut to 128 characters followed by an ellipsis", out.reply)
	}
	if strings.Contains(out.reply, strings.Repeat("k", 129)) {
		t.Errorf("reply repeats more than 128 characters of the caller's key: %d bytes", len(out.reply))
	}
	if detail, _ := out.log["detail"].(string); detail != out.reply {
		t.Errorf("detail = %d bytes, want exactly the reply (%d bytes)", len(detail), len(out.reply))
	}
}

// P5 (danger), part i: only the dedicated type gets verbatim treatment. A plain
// error that happens to use the same words must still be hidden behind the
// generic reply, and its log detail must stay the Go type. This goes red if the
// reply or the log matches on message text instead of on the type.
func TestRequestBodyRejection_SameWordsFromAnUntypedError_StayHidden(t *testing.T) {
	untyped := fmt.Errorf("tool step updateQueue: request body field %q is not a known attribute of operation %q; check the name", "foo", "updateMsgVpnQueue")
	result, logLine := callStubError(t, untyped)

	reply := callToolResultText(t, result, nil)
	if reply != genericInternalMessage {
		t.Errorf("reply = %q, want the generic message for an untyped error", reply)
	}
	detail, _ := logLine["detail"].(string)
	if detail != "*fmt.wrapError" {
		t.Errorf("detail = %q, want only the Go type %q for an untyped error", detail, "*fmt.wrapError")
	}
}
