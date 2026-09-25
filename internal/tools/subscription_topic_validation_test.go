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
	"context"
	"html"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
)

func TestFindHTMLEntity_DetectsEveryRealEscapingShape(t *testing.T) {
	tests := []struct {
		name        string
		topic       string
		want        string // expected findHTMLEntity match, "" if it must not match
		wantLiteral string // html.UnescapeString(want); checked only when want != ""
	}{
		{"reported customer shape", "ABC/&gt;", "&gt;", ">"},
		{"named amp", "A&amp;B/>", "&amp;", "&"},
		{"named lt", "A&lt;B/>", "&lt;", "<"},
		{"named quot", "A&quot;B/>", "&quot;", `"`},
		{"named apos", "A&apos;B/>", "&apos;", "'"},
		{"named all-uppercase, decodes identically", "ABC/&GT;", "&GT;", ">"},
		{"named all-uppercase amp", "ABC/&AMP;", "&AMP;", "&"},
		// The bug this pins: html.UnescapeString decodes "&Gt;" to '≫'
		// (U+226B), not '>' — a blanket case-insensitive match (tried and
		// reverted) would have produced this exact literal in the agent-
		// facing correction message, telling the caller to send a Unicode
		// math symbol instead of the wildcard it actually meant.
		{"mixed-case Gt must NOT match — decodes to a different character", "ABC/&Gt;", "", ""},
		{"mixed-case Lt must NOT match — decodes to a different character", "ABC/&Lt;", "", ""},
		// apos and quot in this single-capital form never decode at all
		// (confirmed against html.UnescapeString) — matching them would
		// have produced a self-contradictory correction ("pass the literal
		// character \"&Apos;\"").
		{"mixed-case Apos must NOT match — never decodes", "ABC/&Apos;", "", ""},
		{"mixed-case Quot must NOT match — never decodes", "ABC/&Quot;", "", ""},
		// apos has no legacy/no-semicolon/uppercase form at all (confirmed:
		// unlike amp/lt/gt/quot, it was never an SGML entity).
		{"uppercase APOS must NOT match — never decodes even with ';'", "ABC/&APOS;", "", ""},
		// Numeric matches never include a real trailing ';' even when the
		// input has one — the terminator (';' or otherwise) is only
		// asserted, never captured (see htmlEntityNumericPattern's doc
		// comment). This is a cosmetic difference only: the literal these
		// decode to is still correct either way, which is what matters.
		{"decimal numeric gt", "ABC/&#62;", "&#62", ">"},
		{"decimal numeric with leading zeros", "ABC/&#062;", "&#062", ">"},
		{"hex numeric gt lowercase", "ABC/&#x3e;", "&#x3e", ">"},
		{"hex numeric gt uppercase", "ABC/&#x3E;", "&#x3E", ">"},
		{"hex numeric with leading zeros", "ABC/&#x03e;", "&#x03e", ">"},
		// HTML5's legacy SGML-compatibility table decodes amp/lt/gt/quot even
		// without a trailing ';' — confirmed live against html.UnescapeString,
		// including mid-word ("&ampersand" -> "&ersand"). A caller who typed
		// the wildcard's escape but dropped the terminator must still be
		// caught, not silently sent to the broker as 7+ literal characters.
		{"named gt without trailing semicolon (legacy leniency)", "ABC/&gt", "&gt", ">"},
		{"named amp without trailing semicolon, mid-word", "A&ampersand/B", "&amp", "&"},
		// apos does NOT get this leniency: it never decodes without ';' at all.
		{"apos without trailing semicolon must NOT match", "ABC/&apos", "", ""},
		// Numeric refs get the same no-semicolon leniency as the named four —
		// html.UnescapeString decodes "&#62"/"&#x3e" with no trailing ';' —
		// but only when nothing that could extend the reference follows.
		{"decimal numeric without trailing semicolon, end of string", "ABC/&#62", "&#62", ">"},
		{"decimal numeric without trailing semicolon, followed by a letter", "ABC/&#62X/Y", "&#62", ">"},
		{"hex numeric without trailing semicolon, end of string", "ABC/&#x3e", "&#x3e", ">"},
		{"hex numeric without trailing semicolon, followed by a non-hex letter", "ABC/&#x3ex/Y", "&#x3e", ">"},
		// The false positive a naive "just drop the ';' requirement" fix would
		// introduce: a further digit (decimal) or hex digit (hex) extends the
		// reference into a real, different, single codepoint — confirmed
		// against html.UnescapeString — not the target codepoint plus a
		// stray trailing character. These must NOT match at all, not match a
		// truncated prefix.
		{"decimal numeric ref must not truncate a longer codepoint (semicolon)", "ABC/&#621;", "", ""},
		{"decimal numeric ref must not truncate a longer codepoint (no semicolon)", "ABC/&#6212/Y", "", ""},
		{"hex numeric ref must not truncate a longer codepoint", "ABC/&#x3e5/Y", "", ""},
		{"literal wildcard, no entity", "ABC/>", "", ""},
		{"literal star wildcard, no entity", "ABC/*", "", ""},
		{"plain topic, no entity", "orders/confirmed", "", ""},
		{"bare ampersand, not an entity", "AT&T/pricing", "", ""},
		{"ampersand followed by word but no semicolon or entity name", "A&ntop/B", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findHTMLEntity(tt.topic)
			if got != tt.want {
				t.Errorf("findHTMLEntity(%q) = %q, want %q", tt.topic, got, tt.want)
			}
			if tt.want != "" {
				if lit := html.UnescapeString(got); lit != tt.wantLiteral {
					t.Errorf("html.UnescapeString(%q) = %q, want %q — the agent-facing correction message would be wrong", got, lit, tt.wantLiteral)
				}
			}
		})
	}
}

func TestValidateHTMLEntityTopic_CleanTopic_ReturnsNil(t *testing.T) {
	err := validateHTMLEntityTopic("create-queue-subscription", map[string]any{
		"msgVpnName":        "default",
		"queueName":         "q1",
		"subscriptionTopic": "ABC/>",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateHTMLEntityTopic_EscapedTopic_ReturnsError(t *testing.T) {
	err := validateHTMLEntityTopic("create-queue-subscription", map[string]any{
		"msgVpnName":        "default",
		"queueName":         "q1",
		"subscriptionTopic": "ABC/&gt;",
	})
	if err == nil {
		t.Fatal("expected a non-nil *htmlEntityTopicError")
	}
	if err.tool != "create-queue-subscription" {
		t.Errorf("tool = %q, want %q", err.tool, "create-queue-subscription")
	}
	if err.param != "subscriptionTopic" {
		t.Errorf("param = %q, want %q", err.param, "subscriptionTopic")
	}
	if err.value != "ABC/&gt;" {
		t.Errorf("value = %q, want %q", err.value, "ABC/&gt;")
	}
	if err.matched != "&gt;" {
		t.Errorf("matched = %q, want %q", err.matched, "&gt;")
	}
	if err.literal != ">" {
		t.Errorf("literal = %q, want %q", err.literal, ">")
	}
}

// TestValidateHTMLEntityTopic_DeleteQueueSubscription_NeverRejects pins the
// review finding (PR #446): delete-queue-subscription is deliberately absent
// from htmlEntityValidatedTools, so an HTML-escaped topic passed to it must
// sail through unrejected — this is the only way an agent can clean up a
// subscription this exact bug created before the fix existed. See
// htmlEntityValidatedTools' doc comment for the full reasoning.
func TestValidateHTMLEntityTopic_DeleteQueueSubscription_NeverRejects(t *testing.T) {
	err := validateHTMLEntityTopic("delete-queue-subscription", map[string]any{
		"msgVpnName":        "default",
		"queueName":         "q1",
		"subscriptionTopic": "ABC/&gt;",
	})
	if err != nil {
		t.Fatalf("delete-queue-subscription must never be rejected by this check, got: %v", err)
	}
}

func TestValidateHTMLEntityTopic_UnrelatedTool_ReturnsNil(t *testing.T) {
	err := validateHTMLEntityTopic("create-queue", map[string]any{
		"msgVpnName": "default",
		"queueName":  "ABC/&gt;", // not a topic-bearing param; must not be checked at all
	})
	if err != nil {
		t.Fatalf("unexpected error for a tool with no registered topic parameter: %v", err)
	}
}

func TestValidateHTMLEntityTopic_NonStringParam_ReturnsNil(t *testing.T) {
	// Not reachable through the real schema (subscriptionTopic is a required
	// string), but this must not panic on a type assertion if it ever is.
	err := validateHTMLEntityTopic("create-queue-subscription", map[string]any{
		"subscriptionTopic": 42,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildHTMLEntityTopicResult_StructuredFields(t *testing.T) {
	entityErr := &htmlEntityTopicError{
		tool:    "create-queue-subscription",
		param:   "subscriptionTopic",
		value:   "ABC/&gt;",
		matched: "&gt;",
		literal: ">",
	}
	result := buildHTMLEntityTopicResult(entityErr)
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map[string]any", result.StructuredContent)
	}
	want := map[string]any{
		"error_source":  "input_validation",
		"parameter":     "subscriptionTopic",
		"value":         "ABC/&gt;",
		"matchedEntity": "&gt;",
		"retryable":     false,
	}
	for k, v := range want {
		if structured[k] != v {
			t.Errorf("structured[%q] = %v, want %v", k, structured[k], v)
		}
	}
	msg, _ := structured["error"].(string)
	for _, want := range []string{"ABC/&gt;", "&gt;", ">", "create-queue-subscription"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention %q", msg, want)
		}
	}
}

// TestCallTool_HTMLEntityTopic_RejectsBeforeHandle confirms the real
// end-to-end wiring: CallTool rejects an HTML-escaped subscriptionTopic in
// its local-validation stage, right after schema validation, and never calls
// the registered handler's Handle at all — matching the schema-validation
// failure path immediately above it in manager.go, not a decorator wrapping
// the handler.
func TestCallTool_HTMLEntityTopic_RejectsBeforeHandle(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("create-queue-subscription")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		t.Fatal("Handle must not be called when the topic is rejected in local validation")
		return nil, nil
	}
	handler.schema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName":        map[string]any{"type": "string"},
			"queueName":         map[string]any{"type": "string"},
			"subscriptionTopic": map[string]any{"type": "string"},
		},
		"required": []string{"msgVpnName", "queueName", "subscriptionTopic"},
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "create-queue-subscription", map[string]any{
		"broker":            "dev",
		"msgVpnName":        "default",
		"queueName":         "q1",
		"subscriptionTopic": "ABC/&gt;",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map[string]any", result.StructuredContent)
	}
	if got := structured["error_source"]; got != "input_validation" {
		t.Errorf("error_source = %v, want %q", got, "input_validation")
	}
}

// TestHTMLEntityValidatedTools_ExpectedToolNames pins the exact map contents
// by name, rather than iterating the map under test: a test that only loops
// over htmlEntityValidatedTools itself would still pass — vacuously — if an
// entry were ever accidentally dropped (found in review, PR #446).
func TestHTMLEntityValidatedTools_ExpectedToolNames(t *testing.T) {
	want := map[string]string{
		"create-queue-subscription": "subscriptionTopic",
	}
	if len(htmlEntityValidatedTools) != len(want) {
		t.Fatalf("htmlEntityValidatedTools has %d entries, want %d: %v", len(htmlEntityValidatedTools), len(want), htmlEntityValidatedTools)
	}
	for tool, param := range want {
		if got, ok := htmlEntityValidatedTools[tool]; !ok || got != param {
			t.Errorf("htmlEntityValidatedTools[%q] = %q, %v; want %q, true", tool, got, ok, param)
		}
	}
	// delete-queue-subscription must stay absent — pinned explicitly (not
	// just by the length check above) since it is the one omission this
	// whole file's design depends on getting right.
	if _, ok := htmlEntityValidatedTools["delete-queue-subscription"]; ok {
		t.Error("delete-queue-subscription must not be in htmlEntityValidatedTools — see its doc comment")
	}
}

// TestHTMLEntityValidatedTools_ParamsExistInRealTool cross-checks
// htmlEntityValidatedTools against the real tool definitions: the map is
// hand-curated (see its doc comment), so a rename of subscriptionTopic, or
// of the tool itself, must fail a test rather than silently leave the
// validator wired to a parameter name that no longer exists. Iterates the
// map under test for its own body (there is nothing to hardcode here beyond
// what TestHTMLEntityValidatedTools_ExpectedToolNames already pins), so it
// verifies shape (existence, type) rather than membership.
func TestHTMLEntityValidatedTools_ParamsExistInRealTool(t *testing.T) {
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	byName := make(map[string]composite.CompositeTool, len(realTools))
	for _, tl := range realTools {
		byName[tl.Name] = tl
	}

	for toolName, param := range htmlEntityValidatedTools {
		tool, ok := byName[toolName]
		if !ok {
			t.Errorf("htmlEntityValidatedTools names %q, which does not exist in the real catalog", toolName)
			continue
		}
		found := false
		for _, p := range tool.Parameters {
			if p.Name == param {
				found = true
				if p.Type != "string" {
					t.Errorf("%s.%s type = %q, want %q", toolName, param, p.Type, "string")
				}
				break
			}
		}
		if !found {
			t.Errorf("%s has no parameter named %q", toolName, param)
		}
	}
}

// TestHTMLEntityValidatedTools_NoUncoveredTopicBearingParam is a drift test
// (suggested in review, PR #446): htmlEntityValidatedTools is hand-curated
// and nothing else in this package structurally ties it to the YAML
// definitions, so a future tool that adds another raw-topic-shaped string
// parameter could ship with no HTML-entity guard at all and nothing would
// fail. Every *Topic-suffixed string parameter in the real catalog must be
// either covered (a value in htmlEntityValidatedTools) or explicitly
// exempted below with a reason — silence is not an option.
//
// The suffix heuristic is deliberately narrow: it must not also catch
// topicEndpointName/topicEndpointConfig (a resource name/config object, not
// a raw topic pattern — and already broker-restricted, see
// htmlEntityValidatedTools' doc comment), which is exactly why those are
// named "topicEndpoint*", not "*Topic".
func TestHTMLEntityValidatedTools_NoUncoveredTopicBearingParam(t *testing.T) {
	// delete-queue-subscription.subscriptionTopic is intentionally exempt —
	// see htmlEntityValidatedTools' doc comment for why guarding delete
	// removes the only non-destructive cleanup path for the exact
	// subscriptions this bug creates.
	exempt := map[string]string{
		"delete-queue-subscription.subscriptionTopic": "removing the guard from delete-queue-subscription would eliminate the only MCP-level recovery path for a subscription this bug already created (SOL-154049, PR #446 review)",
	}

	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	for _, tool := range realTools {
		for _, p := range tool.Parameters {
			if p.Type != "string" || !strings.HasSuffix(p.Name, "Topic") {
				continue
			}
			key := tool.Name + "." + p.Name
			if htmlEntityValidatedTools[tool.Name] == p.Name {
				continue
			}
			if _, ok := exempt[key]; ok {
				continue
			}
			t.Errorf("%s is a string parameter ending in \"Topic\" but is neither in htmlEntityValidatedTools "+
				"nor in this test's exempt list — decide whether it needs the HTML-entity guard and either wire it "+
				"(htmlEntityValidatedTools) or exempt it here with a reason", key)
		}
	}
}
