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

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests exercise withAuthorization in isolation. Each drives a
// hand-built request and asserts on the returned result, whether the stub
// `next` ran, and the emitted "tool authorization" slog line.

// seeded pairs a request with the context auth.PrincipalMiddleware would have
// produced for it in production, so records these tests emit carry identity
// the way real ones do. Written as wrapped(seeded(req)): Go expands the two
// results into the handler's two parameters.
func seeded(req *mcp.CallToolRequest) (context.Context, *mcp.CallToolRequest) {
	return ctxWithPrincipal(context.Background(), req), req
}

// captureSlog swaps slog.Default for a JSON handler writing to buf.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return buf, func() { slog.SetDefault(prev) }
}

// authzLogLines returns every "tool authorization" JSON log line in buf.
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

// policyGranting builds a real *authz.Policy granting each tool to each
// group. Tests use a real Policy so wrapper/policy drift surfaces here.
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

// emptyPolicy builds a policy with no grants — every Authorize returns deny.
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

// requestWithGroups builds a request carrying groups under the Extra key
// the middleware writes on the enabled path.
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

// requestMissingGroupsClaim builds a request with TokenInfo but no groups
// key in Extra — the "IdP omitted the claim" surface.
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
// returns a fixed sentinel result.
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

// Allow: next runs exactly once, wrapper returns its result pointer-identical.
func TestWithAuthorization_Allow_PassesThroughToNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"",
		rec.handler(),
	)

	got, err := wrapped(seeded(requestWithGroups([]string{"Ops"})))
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

// Allow: one INFO "tool authorization" line; decision distinguishable from
// deny/missing (distinguishability, not literal — schema is v1 placeholder).
func TestWithAuthorization_Allow_EmitsInfoAuditWithDistinctDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"",
		newRecordingHandler().handler(),
	)
	if _, err := wrapped(seeded(requestWithGroups([]string{"Ops"}))); err != nil {
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

// Deny: returns (*CallToolResult{IsError:true}, nil) — a tool-level error the
// agent surfaces, not a JSON-RPC error that kills the session.
func TestWithAuthorization_Deny_ReturnsToolLevelErrorResult(t *testing.T) {
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		"",
		newRecordingHandler().handler(),
	)

	got, err := wrapped(seeded(requestWithGroups([]string{"Contractors"})))
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

// Deny: StructuredContent carries error+retryable, TextContent echoes the
// message. retryable=false tells the agent not to loop on the denial.
func TestWithAuthorization_Deny_ResultShapeAndMessage(t *testing.T) {
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		"",
		newRecordingHandler().handler(),
	)
	got, _ := wrapped(seeded(requestWithGroups([]string{"Contractors"})))

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

// Deny security invariant: next is not called; the tool never runs.
func TestWithAuthorization_Deny_DoesNotCallNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(emptyPolicy(t), "delete-queue", "", rec.handler())

	_, _ = wrapped(seeded(requestWithGroups([]string{"Contractors"})))

	if rec.calls != 0 {
		t.Errorf("next called %d times on deny, want 0 (tool must not run on denial)", rec.calls)
	}
}

// Deny: WARN audit line with a decision + decision_reason sub-axis that
// tells the "not permitted" outcome apart from missing-claim.
func TestWithAuthorization_Deny_EmitsWarnAuditWithDistinctDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(emptyPolicy(t), "delete-queue", "", newRecordingHandler().handler())
	_, _ = wrapped(seeded(requestWithGroups([]string{"Contractors"})))

	lines := authzLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), buf.String())
	}
	// Deny is a notable but non-error event: WARN, not INFO (which would
	// bury deny clusters at the same level as normal invocations) and not
	// ERROR (which would flood on-call pagers designed for system failures).
	if got := lines[0]["level"]; got != "WARN" {
		t.Errorf("audit level = %v, want WARN", got)
	}
	decision, _ := lines[0]["decision"].(string)
	if decision != "denied" {
		t.Errorf("decision = %q, want %q", decision, "denied")
	}
	// The sub-axis field distinguishes "not permitted" from missing-claim.
	// It must be present on every deny outcome.
	reason, _ := lines[0]["decision_reason"].(string)
	if reason != "not_permitted" {
		t.Errorf("decision_reason = %q, want %q", reason, "not_permitted")
	}
}

// --- Missing-claim path ---

// Missing-claim: same tool-level-error contract as deny, but with the
// missing-claim message and shape.
func TestWithAuthorization_MissingClaim_ReturnsToolLevelErrorResult(t *testing.T) {
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"",
		newRecordingHandler().handler(),
	)

	got, err := wrapped(seeded(requestMissingGroupsClaim()))
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

// Missing-claim security invariant: next is not called on IdP misconfig.
func TestWithAuthorization_MissingClaim_DoesNotCallNext(t *testing.T) {
	rec := newRecordingHandler()
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"",
		rec.handler(),
	)

	_, _ = wrapped(seeded(requestMissingGroupsClaim()))

	if rec.calls != 0 {
		t.Errorf("next called %d times on missing-claim, want 0", rec.calls)
	}
}

// Missing-claim: decision_reason distinguishable from plain deny so operators
// can tell IdP misconfig from grant miss. Both outcomes share
// decision=denied (they are both non-allows); the sub-axis field carries
// the diagnostic distinction.
func TestWithAuthorization_MissingClaim_EmitsDistinguishableDecision(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"groups",
		newRecordingHandler().handler(),
	)
	_, _ = wrapped(seeded(requestMissingGroupsClaim()))

	lines := authzLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 audit line, got %d: %s", len(lines), buf.String())
	}
	decision, _ := lines[0]["decision"].(string)
	if decision != "denied" {
		t.Errorf("decision = %q, want %q (missing-claim is a non-allow)", decision, "denied")
	}
	// Missing-claim's diagnostic axis is decision_reason. An operator
	// seeing many missing_claim reasons knows to check IdP mapper
	// configuration, not group memberships.
	reason, _ := lines[0]["decision_reason"].(string)
	if reason != "missing_claim" {
		t.Errorf("decision_reason = %q, want %q", reason, "missing_claim")
	}
	// expected_claim carries the configured claim name — the actionable
	// piece of information an operator needs to jump straight to the IdP.
	if got := lines[0]["expected_claim"]; got != "groups" {
		t.Errorf("expected_claim = %v, want %q", got, "groups")
	}
}

// --- Nil/absent TokenInfo path ---

// Nil Extra or nil TokenInfo → treated as missing-claim (no crash, no bypass).
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
				"",
				rec.handler(),
			)
			// Bare context on purpose: no token means no principal, so
			// this is the absent path. Do not wrap it in seeded().
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

// v1 messages are identical — any drift would leak the denial class
// to the caller.
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

// Nil policy is a precondition violation — wrapper must panic (not silently
// allow). Outer withRecovery converts the panic to a visible 500.
func TestWithAuthorization_NilPolicy_Panics(t *testing.T) {
	wrapped := withAuthorization(nil, "get-broker-status", "", newRecordingHandler().handler())

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected wrapper to panic on nil policy; got no panic (silent bypass would be a security fail-open)")
		}
	}()
	_, _ = wrapped(seeded(requestWithGroups([]string{"Ops"})))
}

// Nil policy must panic on every branch — including the branch that would
// otherwise short-circuit to missing-claim before dereferencing policy.
// Without the top-of-closure guard, nil + missing-claim would silently
// return a deny result and the "nil policy is a precondition violation"
// doc claim would hold on only one input shape.
func TestWithAuthorization_NilPolicy_PanicsOnMissingClaimBranchToo(t *testing.T) {
	wrapped := withAuthorization(nil, "get-broker-status", "", newRecordingHandler().handler())

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected wrapper to panic on nil policy even when request has no groups claim; got no panic (doc/code gap)")
		}
	}()
	_, _ = wrapped(seeded(requestMissingGroupsClaim()))
}
