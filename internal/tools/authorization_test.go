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
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/authz"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The wrapper tests exercise withAuthorization in isolation — no MCP SDK
// transport loop, no ToolManager pipeline. Each test drives the wrapper
// with a hand-built *mcp.CallToolRequest and asserts on the returned
// *mcp.CallToolResult, whether the stub `next` handler ran, and the
// slog "tool authorization" audit line the operator sees.

// captureSlog replaces slog.Default with a JSON handler writing to buf,
// returning the buffer plus a cleanup func the test defers. The audit
// line is the operator's window; JSON output lets tests read exact
// field values without regex-fragility.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return buf, func() { slog.SetDefault(prev) }
}

// authzLogLines returns every JSON log line in buf whose msg is
// "tool authorization" — the audit surface the wrapper emits.
func authzLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if entry["msg"] == "tool authorization" {
			out = append(out, entry)
		}
	}
	return out
}

// policyGranting builds a real *authz.Policy that grants each tool in
// tools to each group in groups. The wrapper's tests use a real Policy
// (not a mock) so drift between wrapper and Policy surfaces at test
// time.
func policyGranting(t *testing.T, groups []string, tools ...string) *authz.Policy {
	t.Helper()
	access := make(map[string][]string, len(groups))
	for _, g := range groups {
		access[g] = tools
	}
	p, err := authz.NewPolicy(config.ToolAuthorizationConfig{
		AccessLevelGroups: access,
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

// emptyPolicy builds a policy with no grants. Every Authorize call
// returns Allowed=false.
func emptyPolicy(t *testing.T) *authz.Policy {
	t.Helper()
	p, err := authz.NewPolicy(config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

// requestWithGroups builds a *mcp.CallToolRequest carrying the given
// groups under the SDK-sanctioned Extra key. Mirrors what the auth
// middleware writes on the enabled path.
func requestWithGroups(groups []string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			TokenInfo: &sdkauth.TokenInfo{
				UserID: "alice",
				Extra: map[string]any{
					authz.TokenInfoExtraKeyGroups: groups,
				},
			},
		},
	}
}

// requestMissingGroupsClaim builds a *mcp.CallToolRequest with a
// TokenInfo but no groups key in Extra — the "IdP omitted the claim"
// case the middleware surfaces by leaving the key absent.
func requestMissingGroupsClaim() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			TokenInfo: &sdkauth.TokenInfo{
				UserID: "alice",
				Extra:  map[string]any{},
			},
		},
	}
}

// recordingHandler is a stub `next` handler that records every call and
// returns a fixed sentinel result. Tests use its counter to assert
// whether the wrapper dispatched or short-circuited.
type recordingHandler struct {
	calls    int
	returned *mcp.CallToolResult
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{
		returned: &mcp.CallToolResult{
			StructuredContent: map[string]any{"sentinel": true},
		},
	}
}

func (r *recordingHandler) handler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		r.calls++
		return r.returned, nil
	}
}

// --- Allow path ---

// TestWithAuthorization_Allow_PassesThroughToNext pins the caller-facing
// contract on the allow branch: the wrapper returns what next returned
// (same pointer) and next runs exactly once. Any drift here breaks
// every RBAC deployment's happy path.
func TestWithAuthorization_Allow_PassesThroughToNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		rec.handler(),
	)

	got, err := wrapped(context.Background(), requestWithGroups([]string{"Ops"}))
	if err != nil {
		t.Fatalf("wrapper returned error on allow: %v", err)
	}
	if got != rec.returned {
		t.Errorf("wrapper did not pass next's result through; got %p, want %p", got, rec.returned)
	}
	if rec.calls != 1 {
		t.Errorf("next called %d times, want exactly 1", rec.calls)
	}
}

// TestWithAuthorization_Allow_EmitsInfoAuditWithDistinctDecision pins
// the operator-facing contract: allow decisions emit an INFO
// "tool authorization" audit line naming the tool and carrying a
// decision value distinguishable from deny and missing-claim. The
// exact decision string schema is finalized in the audit-refinement
// follow-up ticket; this test locks distinguishability, not the
// literal.
func TestWithAuthorization_Allow_EmitsInfoAuditWithDistinctDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		newRecordingHandler().handler(),
	)
	if _, err := wrapped(context.Background(), requestWithGroups([]string{"Ops"})); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	lines := authzLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), buf.String())
	}
	if got := lines[0]["level"]; got != "INFO" {
		t.Errorf("audit level = %v, want INFO", got)
	}
	if got := lines[0]["tool"]; got != "get-broker-status" {
		t.Errorf("audit tool = %v, want get-broker-status", got)
	}
	decision, ok := lines[0]["decision"].(string)
	if !ok || decision == "" {
		t.Fatalf("audit line missing decision field: %v", lines[0])
	}
	// Allow's decision must not collide with either deny outcome — the
	// operator needs to grep for each independently.
	if decision == "denied" || decision == "denied_missing_claim" {
		t.Errorf("allow decision %q collides with a deny decision value; must be distinguishable", decision)
	}
}

// --- Deny path ---

// TestWithAuthorization_Deny_ReturnsToolLevelErrorResult pins the
// caller-facing distinction between tool-level errors and JSON-RPC
// protocol errors: denials must return (*CallToolResult{IsError:true},
// nil), not (nil, error). MCP clients treat a Go error return as a
// session-level failure; the tool-level error result is what tells the
// agent "the tool ran and produced an error outcome" — which is what
// a policy denial is semantically.
func TestWithAuthorization_Deny_ReturnsToolLevelErrorResult(t *testing.T) {
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		newRecordingHandler().handler(),
	)

	got, err := wrapped(context.Background(), requestWithGroups([]string{"Contractors"}))
	if err != nil {
		t.Fatalf("wrapper returned Go error on deny; want tool-level result. err=%v", err)
	}
	if got == nil {
		t.Fatal("wrapper returned nil result on deny")
	}
	if !got.IsError {
		t.Errorf("deny result IsError = false, want true")
	}
}

// TestWithAuthorization_Deny_ResultShapeAndMessage pins the caller-
// facing payload: StructuredContent carries error+retryable with the
// uniform authzDeniedMessage, and TextContent echoes the same message.
// The retryable=false value tells the agent not to loop on the same
// denied call.
func TestWithAuthorization_Deny_ResultShapeAndMessage(t *testing.T) {
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		newRecordingHandler().handler(),
	)
	got, _ := wrapped(context.Background(), requestWithGroups([]string{"Contractors"}))

	sc, ok := got.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is not map[string]any: %T", got.StructuredContent)
	}
	if sc["error"] != authzDeniedMessage {
		t.Errorf("StructuredContent.error = %v, want authzDeniedMessage %q", sc["error"], authzDeniedMessage)
	}
	if sc["retryable"] != false {
		t.Errorf("StructuredContent.retryable = %v, want false", sc["retryable"])
	}
	if len(got.Content) != 1 {
		t.Fatalf("expected exactly 1 Content block, got %d", len(got.Content))
	}
	tc, ok := got.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] is not *TextContent: %T", got.Content[0])
	}
	if tc.Text != authzDeniedMessage {
		t.Errorf("TextContent.Text = %q, want authzDeniedMessage %q", tc.Text, authzDeniedMessage)
	}
}

// TestWithAuthorization_Deny_DoesNotCallNext is the security invariant:
// a denied call must not reach the tool handler. If next ran on deny,
// the tool's side effects would happen despite the denial.
func TestWithAuthorization_Deny_DoesNotCallNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(emptyPolicy(t), "delete-queue", rec.handler())

	_, _ = wrapped(context.Background(), requestWithGroups([]string{"Contractors"}))

	if rec.calls != 0 {
		t.Errorf("next called %d times on deny, want 0 (tool must not run on denial)", rec.calls)
	}
}

// TestWithAuthorization_Deny_EmitsInfoAuditWithDistinctDecision pins
// the operator's ability to distinguish deny events from allow and
// missing-claim events in the log stream.
func TestWithAuthorization_Deny_EmitsInfoAuditWithDistinctDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(emptyPolicy(t), "delete-queue", newRecordingHandler().handler())
	_, _ = wrapped(context.Background(), requestWithGroups([]string{"Contractors"}))

	lines := authzLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), buf.String())
	}
	if got := lines[0]["level"]; got != "INFO" {
		t.Errorf("audit level = %v, want INFO", got)
	}
	decision, _ := lines[0]["decision"].(string)
	if decision == "" {
		t.Fatalf("audit line missing decision: %v", lines[0])
	}
	// Deny's decision must be distinct from missing-claim so operators
	// can tell "IdP omitted the claim" from "the claim was there but
	// didn't grant the tool."
	if decision == "denied_missing_claim" || strings.Contains(strings.ToLower(decision), "allow") {
		t.Errorf("deny decision %q collides with another outcome value", decision)
	}
}

// --- Missing-claim path ---

// TestWithAuthorization_MissingClaim_ReturnsToolLevelErrorResult pins
// the same tool-level-error contract as the deny path for the missing-
// claim branch. The two branches share result-shape code but the
// upstream trigger differs — locking both preserves the wrapper's
// two-branch structure against a refactor that accidentally collapses
// one into the other.
func TestWithAuthorization_MissingClaim_ReturnsToolLevelErrorResult(t *testing.T) {
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		newRecordingHandler().handler(),
	)

	got, err := wrapped(context.Background(), requestMissingGroupsClaim())
	if err != nil {
		t.Fatalf("wrapper returned Go error on missing-claim: %v", err)
	}
	if got == nil || !got.IsError {
		t.Fatalf("missing-claim result IsError = %v, want true", got)
	}
	sc, _ := got.StructuredContent.(map[string]any)
	if sc["error"] != authzMissingClaimMessage {
		t.Errorf("StructuredContent.error = %v, want authzMissingClaimMessage %q", sc["error"], authzMissingClaimMessage)
	}
	if sc["retryable"] != false {
		t.Errorf("StructuredContent.retryable = %v, want false", sc["retryable"])
	}
	tc, ok := got.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != authzMissingClaimMessage {
		t.Errorf("TextContent.Text = %v, want authzMissingClaimMessage %q", got.Content[0], authzMissingClaimMessage)
	}
}

// TestWithAuthorization_MissingClaim_DoesNotCallNext is the security
// invariant for the missing-claim branch: an IdP misconfiguration
// (token missing the groups claim) must not silently succeed as if
// the caller were in every group.
func TestWithAuthorization_MissingClaim_DoesNotCallNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		rec.handler(),
	)

	_, _ = wrapped(context.Background(), requestMissingGroupsClaim())

	if rec.calls != 0 {
		t.Errorf("next called %d times on missing-claim, want 0", rec.calls)
	}
}

// TestWithAuthorization_MissingClaim_EmitsDistinguishableDecision pins
// the operator's ability to tell "IdP misconfigured (no groups
// claim)" from "caller's groups don't grant this tool" — two failure
// classes requiring different remediations.
func TestWithAuthorization_MissingClaim_EmitsDistinguishableDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		newRecordingHandler().handler(),
	)
	_, _ = wrapped(context.Background(), requestMissingGroupsClaim())

	lines := authzLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), buf.String())
	}
	decision, _ := lines[0]["decision"].(string)
	if decision == "" {
		t.Fatalf("audit line missing decision: %v", lines[0])
	}
	// Missing-claim must be distinguishable from a plain deny — an
	// operator seeing many missing-claim events knows to check IdP
	// mapper configuration, not group memberships.
	if decision == "denied" {
		t.Errorf("missing-claim decision must differ from plain deny; both were %q", decision)
	}
	if strings.Contains(strings.ToLower(decision), "allow") {
		t.Errorf("missing-claim decision %q looks like an allow value", decision)
	}
}

// --- Nil/absent TokenInfo path ---

// TestWithAuthorization_NilTokenInfo_TreatsAsMissingClaim covers the
// bare-CallToolRequest case that test scaffolding and disabled-auth
// deployments hit: req.Extra or req.Extra.TokenInfo is nil. The
// wrapper must treat this as missing-claim rather than crashing or
// silently allowing.
func TestWithAuthorization_NilTokenInfo_TreatsAsMissingClaim(t *testing.T) {
	cases := []struct {
		name string
		req  *mcp.CallToolRequest
	}{
		{"nil Extra", &mcp.CallToolRequest{}},
		{"nil TokenInfo", &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingHandler()
			wrapped := withAuthorization(
				policyGranting(t, []string{"Ops"}, "get-broker-status"),
				"get-broker-status",
				rec.handler(),
			)
			got, err := wrapped(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("wrapper returned Go error: %v", err)
			}
			if got == nil || !got.IsError {
				t.Fatalf("expected tool-level error result, got %v", got)
			}
			sc, _ := got.StructuredContent.(map[string]any)
			if sc["error"] != authzMissingClaimMessage {
				t.Errorf("expected missing-claim message, got %v", sc["error"])
			}
			if rec.calls != 0 {
				t.Errorf("next called %d times, want 0", rec.calls)
			}
		})
	}
}

// --- Message constants ---

// TestAuthzMessages_IdenticalForV1 locks in the v1 design decision
// that deny and missing-claim callers see the same string. Any drift
// (one message adds detail, the other doesn't) would leak the class
// of denial to the caller — exactly the caller-side disclosure
// discipline that keeps configuration metadata operator-only.
func TestAuthzMessages_IdenticalForV1(t *testing.T) {
	if authzDeniedMessage != authzMissingClaimMessage {
		t.Errorf("v1 requires identical caller-facing messages; got denied=%q, missing=%q",
			authzDeniedMessage, authzMissingClaimMessage)
	}
	if authzDeniedMessage == "" {
		t.Error("authzDeniedMessage must not be empty; callers see this string")
	}
}

// --- Precondition ---

// TestWithAuthorization_NilPolicy_Panics locks the fail-loud
// precondition: composing the wrapper with a nil policy is a
// programmer error. If the wrapper silently allowed on nil, every
// RBAC-enabled deployment could accidentally bypass authorization
// after a composition-site refactor — the exact silent-fail-open
// class the feature exists to prevent. Panic + outer withRecovery
// converts this to a visible 500 rather than a security regression.
func TestWithAuthorization_NilPolicy_Panics(t *testing.T) {
	wrapped := withAuthorization(nil, "get-broker-status", newRecordingHandler().handler())

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected wrapper to panic on nil policy; got no panic (silent bypass would be a security fail-open)")
		}
	}()
	_, _ = wrapped(context.Background(), requestWithGroups([]string{"Ops"}))
}
