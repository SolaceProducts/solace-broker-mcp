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

// The tests in this file pin the audit event's emitted schema — what
// operators, SIEM parsers, and compliance auditors observe on the
// "tool authorization" slog record. Each test drives the wrapper
// through one outcome (allow / deny / missing-claim) and asserts on
// the captured record. Negative assertions here are load-bearing:
// caller_groups absence on deny is a privacy contract; the absence
// of matched_groups* fields on missing-claim is a schema-legibility
// contract; both must fail loudly on any silent regression.

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// installCorrelationCapture wraps a JSON slog handler with the correlation
// slog handler decorator and installs it as slog.Default. Tests that seed
// a correlation ID on the request context can observe it stamped onto
// records the same way production observes it — the wrapper never adds
// correlation_id manually, so this fixture is the only way to confirm
// the field lands where operators grep for it.
func installCorrelationCapture(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	handler := correlation.NewSlogHandler(base)
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	return buf, func() { slog.SetDefault(prev) }
}

// parseAuthzRecord parses the captured buffer, expects exactly one
// "tool authorization" JSON record, and returns it as a decoded map.
// Multi-line or non-JSON output fails the test.
func parseAuthzRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var records []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("non-JSON log line: %q\nerr: %v", raw, err)
		}
		if m["msg"] == "tool authorization" {
			records = append(records, m)
		}
	}
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 audit record, got %d\nbuf:\n%s", len(records), buf.String())
	}
	return records[0]
}

// hasKey reports whether the record has a top-level attribute under key.
// Negative-assertion tests use this to check that absent-by-schema fields
// truly are absent — not present-with-zero-value.
// assertRecordIdentity asserts the four identity fields carry the fixture's
// values. Presence alone is not enough: the failure mode that matters is a
// record with the right shape naming the wrong caller, which the previous
// hasKey loops accepted.
func assertRecordIdentity(t *testing.T, rec map[string]any, kind, wantJTI string) {
	t.Helper()
	want := map[string]string{
		"sub":       "alice",
		"iss":       "https://idp.example.com",
		"client_id": "cursor-ide",
		"jti":       wantJTI,
	}
	for k, v := range want {
		got, ok := rec[k]
		if !ok {
			t.Errorf("identity field %q missing from %s record", k, kind)
			continue
		}
		if got != v {
			t.Errorf("%s record: %s = %v, want %q", kind, k, got, v)
		}
	}
}

func hasKey(record map[string]any, key string) bool {
	_, ok := record[key]
	return ok
}

// requestWithGroupsAndCorrelation is requestWithGroups plus a seeded
// correlation ID on the context — used only by tests that assert on the
// correlation_id field. The correlation handler reads the ID from the
// record's context (which slog.LogAttrs derives from the ctx passed by
// the wrapper), not from any wrapper-side attribute.
func requestWithGroupsAndCorrelation(groups []string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			TokenInfo: &sdkauth.TokenInfo{
				UserID: "alice",
				Extra: map[string]any{
					"iss":                          "https://idp.example.com",
					"client_id":                    "cursor-ide",
					"jti":                          "jti-audit-1",
					authz.TokenInfoExtraKeyGroups:  groups,
				},
			},
		},
	}
}

// requestMissingClaimWithCorrelation is requestMissingGroupsClaim's
// counterpart carrying non-empty audit identity fields so identity-field
// assertions in this file don't collapse to the <absent> sentinel.
func requestMissingClaimWithCorrelation() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			TokenInfo: &sdkauth.TokenInfo{
				UserID: "alice",
				Extra: map[string]any{
					"iss":       "https://idp.example.com",
					"client_id": "cursor-ide",
					"jti":       "jti-audit-2",
				},
			},
		},
	}
}

// ctxWithPrincipal stands in for auth.InjectPrincipal: identity comes from the
// Principal on ctx, not req.Extra.TokenInfo (SOL-152087), so tests asserting
// identity fields must seed ctx the way the middleware would.
func ctxWithPrincipal(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	var info *sdkauth.TokenInfo
	if req.Extra != nil {
		info = req.Extra.TokenInfo
	}
	return auth.WithPrincipal(ctx, auth.NewPrincipal(ctx, info))
}

// TestAuditEvent_Allow_EmitsInfoWithFullSchema pins the complete allow
// record: level, decision, matched-groups triplet, identity, correlation
// ID, and — as negative assertions — the absence of every field that
// belongs to a deny or missing-claim outcome. Reviewers looking for "what
// SHOULD be on an allow line" find the full schema in one place; a
// contributor tempted to add decision_reason=allowed as a "harmless
// consistency" change would fail here loudly.
func TestAuditEvent_Allow_EmitsInfoWithFullSchema(t *testing.T) {
	buf, restore := installCorrelationCapture(t)
	defer restore()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "list-queues"),
		"list-queues",
		"groups",
		newRecordingHandler().handler(),
	)
	req := requestWithGroupsAndCorrelation([]string{"Ops"})
	ctx := ctxWithPrincipal(correlation.With(context.Background(), "corr-allow"), req)
	if _, err := wrapped(ctx, req); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	rec := parseAuthzRecord(t, buf)

	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["decision"] != "allowed" {
		t.Errorf("decision = %v, want %q", rec["decision"], "allowed")
	}
	if rec["tool"] != "list-queues" {
		t.Errorf("tool = %v, want %q", rec["tool"], "list-queues")
	}
	mg, ok := rec["matched_groups"].([]any)
	if !ok || len(mg) != 1 || mg[0] != "Ops" {
		t.Errorf("matched_groups = %v, want [\"Ops\"]", rec["matched_groups"])
	}
	if got := rec["matched_groups_total"]; got != float64(1) {
		t.Errorf("matched_groups_total = %v, want 1", got)
	}
	if got := rec["matched_groups_truncated"]; got != false {
		t.Errorf("matched_groups_truncated = %v, want false", got)
	}
	assertRecordIdentity(t, rec, "allow", "jti-audit-1")
	if rec["correlation_id"] != "corr-allow" {
		t.Errorf("correlation_id = %v, want %q (stamped by correlation slog handler, not by the wrapper)", rec["correlation_id"], "corr-allow")
	}
	if hasKey(rec, "decision_reason") {
		t.Errorf("decision_reason must NOT appear on allow records; got %v", rec["decision_reason"])
	}
	if hasKey(rec, "expected_claim") {
		t.Errorf("expected_claim must NOT appear on allow records; got %v", rec["expected_claim"])
	}
	if hasKey(rec, "caller_groups") {
		t.Errorf("caller_groups must NOT appear on allow records; got %v", rec["caller_groups"])
	}
}

// TestAuditEvent_Deny_EmitsWarnWithNotPermittedAndNoCallerGroups pins the
// deny record's schema AND — critically — the absence of caller_groups.
// The absence is a load-bearing separation-of-duties decision: MCP
// operators reading these logs are not necessarily entitled to see the
// caller's full IdP group membership. A future contributor adding
// caller_groups "for debuggability" would regress a privacy boundary; the
// negative assertion below fails loudly if that happens.
func TestAuditEvent_Deny_EmitsWarnWithNotPermittedAndNoCallerGroups(t *testing.T) {
	buf, restore := installCorrelationCapture(t)
	defer restore()

	// Grant list-queues to Ops; caller is in OtherGroup and asks for
	// delete-queue. Both dimensions miss.
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "list-queues"),
		"delete-queue",
		"groups",
		newRecordingHandler().handler(),
	)
	req := requestWithGroupsAndCorrelation([]string{"OtherGroup"})
	ctx := ctxWithPrincipal(correlation.With(context.Background(), "corr-deny"), req)
	if _, err := wrapped(ctx, req); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	rec := parseAuthzRecord(t, buf)

	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["decision"] != "denied" {
		t.Errorf("decision = %v, want %q", rec["decision"], "denied")
	}
	if rec["decision_reason"] != "not_permitted" {
		t.Errorf("decision_reason = %v, want %q", rec["decision_reason"], "not_permitted")
	}
	mg, ok := rec["matched_groups"].([]any)
	if !ok || len(mg) != 0 {
		t.Errorf("matched_groups = %v, want [] (schema-uniform with allow but always empty on deny)", rec["matched_groups"])
	}
	if got := rec["matched_groups_total"]; got != float64(0) {
		t.Errorf("matched_groups_total = %v, want 0", got)
	}
	if got := rec["matched_groups_truncated"]; got != false {
		t.Errorf("matched_groups_truncated = %v, want false", got)
	}
	assertRecordIdentity(t, rec, "deny", "jti-audit-1")
	if rec["correlation_id"] != "corr-deny" {
		t.Errorf("correlation_id = %v, want %q", rec["correlation_id"], "corr-deny")
	}
	if hasKey(rec, "expected_claim") {
		t.Errorf("expected_claim must NOT appear on deny records; got %v", rec["expected_claim"])
	}
	// LOAD-BEARING: the caller's actual (extracted) groups list must
	// never appear on deny events. If this assertion fails, a privacy
	// boundary was silently regressed; consult the PR description for
	// the audit-log disclosure discipline before "fixing" the test.
	if hasKey(rec, "caller_groups") {
		t.Errorf("caller_groups must NEVER appear on deny records — privacy contract regression: %v", rec["caller_groups"])
	}
}

// TestAuditEvent_MissingClaim_EmitsWarnWithExpectedClaimAndNoMatchedGroupsFields
// pins the missing-claim record's schema. It asserts the presence of
// expected_claim (the diagnostic field an operator jumps to the IdP with)
// AND the absence of every matched_groups* field: on missing-claim there
// is no groups list to describe, so empty-set/zero would be dead noise
// that dilutes the signal on the outcome operators actually care about.
func TestAuditEvent_MissingClaim_EmitsWarnWithExpectedClaimAndNoMatchedGroupsFields(t *testing.T) {
	buf, restore := installCorrelationCapture(t)
	defer restore()

	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "list-queues"),
		"list-queues",
		"groups",
		newRecordingHandler().handler(),
	)
	req := requestMissingClaimWithCorrelation()
	ctx := ctxWithPrincipal(correlation.With(context.Background(), "corr-miss"), req)
	if _, err := wrapped(ctx, req); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	rec := parseAuthzRecord(t, buf)

	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["decision"] != "denied" {
		t.Errorf("decision = %v, want %q (missing-claim is a non-allow)", rec["decision"], "denied")
	}
	if rec["decision_reason"] != "missing_claim" {
		t.Errorf("decision_reason = %v, want %q", rec["decision_reason"], "missing_claim")
	}
	if rec["expected_claim"] != "groups" {
		t.Errorf("expected_claim = %v, want %q", rec["expected_claim"], "groups")
	}
	assertRecordIdentity(t, rec, "missing-claim", "jti-audit-2")
	if rec["correlation_id"] != "corr-miss" {
		t.Errorf("correlation_id = %v, want %q", rec["correlation_id"], "corr-miss")
	}
	// The three matched_groups* fields carry no signal on missing-claim
	// — the caller had no groups slice at all. Emitting empty/zero here
	// would blur the distinction between "we found groups but none
	// granted" (deny) and "we found no groups to begin with" (missing).
	for _, k := range []string{"matched_groups", "matched_groups_total", "matched_groups_truncated"} {
		if hasKey(rec, k) {
			t.Errorf("%q must NOT appear on missing-claim records (fields carry signal where they belong); got %v", k, rec[k])
		}
	}
	if hasKey(rec, "caller_groups") {
		t.Errorf("caller_groups must NEVER appear on missing-claim records — privacy contract regression: %v", rec["caller_groups"])
	}
}

// TestAuditEvent_MissingClaim_SanitizesExpectedClaim pins that the
// configured claim name passes through sanitize.Claim before hitting the
// slog record. Admin-authored strings reaching a log field must be
// sanitized — a claim name pasted with a stray zero-width joiner (or
// worse, an ANSI escape or a newline) would otherwise leak into logs
// verbatim. This test uses a ZWJ (U+200D) as the canary control
// character; sanitize.Claim strips it. A future refactor that dropped
// the sanitize.Claim call would fail here.
func TestAuditEvent_MissingClaim_SanitizesExpectedClaim(t *testing.T) {
	buf, restore := installCorrelationCapture(t)
	defer restore()

	// "groups" + zero-width joiner (U+200D) — U+200D is Unicode category
	// Cf and is stripped by sanitize.Claim. The claim name is admin-
	// authored, so this is the exact defense the sanitize call protects.
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "list-queues"),
		"list-queues",
		"groups\u200d",
		newRecordingHandler().handler(),
	)
	missingReq := requestMissingClaimWithCorrelation()
	if _, err := wrapped(ctxWithPrincipal(context.Background(), missingReq), missingReq); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	rec := parseAuthzRecord(t, buf)
	if rec["expected_claim"] != "groups" {
		t.Errorf("expected_claim = %q, want %q — sanitize.Claim must strip Cf characters before emission", rec["expected_claim"], "groups")
	}
}

// TestAuditEvent_Allow_SanitizesMatchedGroupsElements pins that EVERY
// element of matched_groups is sanitized, not just the first. Uses two
// admin-configured group names each carrying a Cf character; both must
// survive the config validator (whitespace is rejected by validate() but
// Cf is not) and both must be sanitized before slog sees them. A future
// refactor with a "sanitize the first, skip the rest" bug would fail
// here; a refactor that dropped sanitize.Claim entirely would fail here.
func TestAuditEvent_Allow_SanitizesMatchedGroupsElements(t *testing.T) {
	buf, restore := installCorrelationCapture(t)
	defer restore()

	// Both group names carry a zero-width joiner. Authorize matches
	// byte-exactly against these names — a caller carrying the same
	// bytes lands in both groups. Post-sanitize, the ZWJ is stripped
	// so the emitted matched_groups shows the clean names.
	group1 := "Ops\u200d"
	group2 := "Reviewers\u200d"
	cfg := config.ToolAuthorizationConfig{
		AccessLevelGroups: map[string][]string{
			group1: {"list-queues"},
			group2: {"list-queues"},
		},
	}
	policy, err := authz.NewPolicy(cfg)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	wrapped := withAuthorization(policy, "list-queues", "groups", newRecordingHandler().handler())
	if _, err := wrapped(context.Background(), requestWithGroupsAndCorrelation([]string{group1, group2})); err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}

	rec := parseAuthzRecord(t, buf)
	mg, ok := rec["matched_groups"].([]any)
	if !ok {
		t.Fatalf("matched_groups is not an array: %v", rec["matched_groups"])
	}
	got := make(map[string]bool, len(mg))
	for _, v := range mg {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	if !got["Ops"] || !got["Reviewers"] {
		t.Errorf("matched_groups = %v, want both %q and %q after sanitization (every element must be sanitized, not just the first)", mg, "Ops", "Reviewers")
	}
	// Negative: the raw ZWJ-containing names must NOT appear.
	if got[group1] || got[group2] {
		t.Errorf("matched_groups contained un-sanitized element(s); raw values leaked into audit log: %v", mg)
	}
}

// TestAuditEvent_MatchedGroupsBounding pins the three axes of the
// bounding contract: matched_groups_total records the true count
// (unbounded), matched_groups is capped at exactly the bound, and
// matched_groups_truncated flips true only when there is at least one
// group in excess of the bound. Table-driven across three scenarios:
// bound-under (16), at-bound (32, inclusive-cap boundary), and over-bound
// by one (33). The at-bound row is load-bearing: a naive impl that
// truncates when total >= bound would spuriously flip the truncated flag
// at exactly 32.
func TestAuditEvent_MatchedGroupsBounding(t *testing.T) {
	// bound is copied here rather than imported from the wrapper — the
	// constant is unexported and its value is a contract, not an internal
	// detail. If the wrapper's bound moves off 32 without the docstrings
	// doc changing, this test fails and points at the drift.
	const bound = 32
	cases := []struct {
		name          string
		total         int
		wantLen       int
		wantTruncated bool
	}{
		{"bound_under_16", 16, 16, false},
		{"at_bound_inclusive_32", 32, 32, false},
		{"one_over_bound_33", 33, 32, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, restore := installCorrelationCapture(t)
			defer restore()

			// Build a policy where N distinct groups each grant the same
			// tool. Caller carries all N groups → union match = all N.
			groups := make([]string, c.total)
			access := make(map[string][]string, c.total)
			for i := 0; i < c.total; i++ {
				name := fmt.Sprintf("Group%03d", i)
				groups[i] = name
				access[name] = []string{"list-queues"}
			}
			policy, err := authz.NewPolicy(config.ToolAuthorizationConfig{AccessLevelGroups: access})
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			wrapped := withAuthorization(policy, "list-queues", "groups", newRecordingHandler().handler())
			boundReq := requestWithGroupsAndCorrelation(groups)
	if _, err := wrapped(ctxWithPrincipal(context.Background(), boundReq), boundReq); err != nil {
				t.Fatalf("wrapper returned error: %v", err)
			}

			rec := parseAuthzRecord(t, buf)
			if got := rec["matched_groups_total"]; got != float64(c.total) {
				t.Errorf("matched_groups_total = %v, want %d (unbounded true count)", got, c.total)
			}
			if got := rec["matched_groups_truncated"]; got != c.wantTruncated {
				t.Errorf("matched_groups_truncated = %v, want %v (flips true only when total > bound)", got, c.wantTruncated)
			}
			mg, ok := rec["matched_groups"].([]any)
			if !ok {
				t.Fatalf("matched_groups is not an array: %v", rec["matched_groups"])
			}
			if len(mg) != c.wantLen {
				t.Errorf("len(matched_groups) = %d, want %d (cap = %d, inclusive)", len(mg), c.wantLen, bound)
			}
			// When truncation fires, every returned element must still be
			// one of the input group names — no synthetic filler, no
			// duplicates. Set-membership check works here because the
			// input names are unique.
			seen := make(map[string]bool, len(mg))
			inputSet := make(map[string]bool, len(groups))
			for _, g := range groups {
				inputSet[g] = true
			}
			for _, v := range mg {
				s, isString := v.(string)
				if !isString {
					t.Errorf("matched_groups contained non-string element: %v", v)
					continue
				}
				if !inputSet[s] {
					t.Errorf("matched_groups contained unexpected element %q — not in the caller's group list", s)
				}
				if seen[s] {
					t.Errorf("matched_groups contained duplicate element %q — dedup contract violated", s)
				}
				seen[s] = true
			}
		})
	}
}
