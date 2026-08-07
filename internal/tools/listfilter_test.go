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
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests exercise WithListFiltering in isolation. Each drives a hand-built
// request through the middleware over a stub `next`, then asserts on BOTH
// halves of the contract: the tools the caller gets back, and the record the
// operator sees.
//
// The two are asserted together deliberately. Nothing about the filtering
// reaches the caller — a zero-result list is a normal 200 — so the audit record
// is the only diagnostic this feature has. A test that checked the returned
// list alone could pass while logging the wrong reason, which is precisely the
// failure an operator would hit.
//
// Policies are real (see policyGranting in authorization_test.go): the whole
// design rests on this filter and withAuthorization reaching identical
// verdicts, and a stubbed policy would let them drift silently.

// logLinesWithMsg returns every JSON record in buf whose msg equals want.
// authzLogLines is the call path's equivalent, with its message hardcoded.
func logLinesWithMsg(t *testing.T, buf *bytes.Buffer, want string) []map[string]any {
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
		if entry["msg"] == want {
			out = append(out, entry)
		}
	}
	return out
}

// listFilterLogLines returns every per-request filter record in buf.
func listFilterLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	return logLinesWithMsg(t, buf, "tool list filter")
}

// listFilterErrorLines returns the SDK-contract-violation records.
func listFilterErrorLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	return logLinesWithMsg(t, buf,
		"internal: tools/list result contained nil tools — SDK contract violation")
}

// listToolsRequest builds a tools/list request carrying groups under the Extra
// key the auth middleware writes on the enabled path.
func listToolsRequest(groups []string) *mcp.ServerRequest[*mcp.ListToolsParams] {
	return &mcp.ServerRequest[*mcp.ListToolsParams]{
		Params: &mcp.ListToolsParams{},
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

// listToolsRequestMissingClaim builds a request whose token verified but
// carries no groups key — the IdP-misconfiguration surface the spike hit.
func listToolsRequestMissingClaim() *mcp.ServerRequest[*mcp.ListToolsParams] {
	return &mcp.ServerRequest[*mcp.ListToolsParams]{
		Params: &mcp.ListToolsParams{},
		Extra: &mcp.RequestExtra{
			TokenInfo: &sdkauth.TokenInfo{
				UserID: "alice",
				Extra:  map[string]any{},
			},
		},
	}
}

// toolsResult builds the unfiltered ListToolsResult a stub `next` returns.
func toolsResult(names ...string) *mcp.ListToolsResult {
	out := &mcp.ListToolsResult{Tools: make([]*mcp.Tool, 0, len(names))}
	for _, n := range names {
		out.Tools = append(out.Tools, &mcp.Tool{Name: n})
	}
	return out
}

// listHandler returns a stub `next` yielding res, and a pointer to a call
// counter so a test can prove dispatch happened exactly once.
func listHandler(res mcp.Result, err error) (mcp.MethodHandler, *int) {
	calls := 0
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		calls++
		return res, err
	}, &calls
}

// toolNames extracts and sorts the names in a result, so assertions do not
// depend on registration order.
func toolNames(t *testing.T, res mcp.Result) []string {
	t.Helper()
	ltr, ok := res.(*mcp.ListToolsResult)
	if !ok {
		t.Fatalf("result is %T, want *mcp.ListToolsResult", res)
	}
	out := make([]string, 0, len(ltr.Tools))
	for _, tool := range ltr.Tools {
		out = append(out, tool.Name)
	}
	sort.Strings(out)
	return out
}

// assertNames fails unless got holds exactly want, order-insensitively.
func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("tools = %v, want %v", got, sorted)
		}
	}
}

// assertOneRecord fails unless exactly one filter record was emitted, and
// returns it. Every filtered list must produce exactly one — no more, so an
// operator counting records can count requests.
func assertOneRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	records := listFilterLogLines(t, buf)
	if len(records) != 1 {
		t.Fatalf("emitted %d filter records, want exactly 1: %v", len(records), records)
	}
	return records[0]
}

// assertRecord checks the two fields an operator triages on, plus the event tag
// that separates these records from the call path's.
func assertRecord(t *testing.T, rec map[string]any, wantReason, wantLevel string) {
	t.Helper()
	if got := rec["decision_reason"]; got != wantReason {
		t.Errorf("decision_reason = %v, want %q", got, wantReason)
	}
	if got := rec["level"]; got != wantLevel {
		t.Errorf("level = %v, want %q", got, wantLevel)
	}
	if got := rec["event"]; got != eventToolListFilter {
		t.Errorf("event = %v, want %q", got, eventToolListFilter)
	}
}

// --- Filtering: what the caller gets, and what the operator sees ---

// A caller whose group grants some tools sees exactly those plus the exemption,
// and the record reads as a normal partial filter.
func TestWithListFiltering_GrantingGroup_KeepsGrantedPlusExempt(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	next, calls := listHandler(
		toolsResult(listBrokersToolName, "get-broker-status", "list-queues", "delete-queue"), nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status", "list-queues"),
	)(next)

	res, err := wrapped(context.Background(), methodToolsList, listToolsRequest([]string{"Ops"}))
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("next called %d times, want 1", *calls)
	}

	assertNames(t, toolNames(t, res),
		[]string{listBrokersToolName, "get-broker-status", "list-queues"})
	assertRecord(t, assertOneRecord(t, buf), listReasonFiltered, "INFO")
}

// Groups that match no grant leave the exemption alone. This is the policy
// working as configured, so it is INFO — not an error, and not a 401.
func TestWithListFiltering_NoMatchingGrants_KeepsExemptOnly(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	next, _ := listHandler(
		toolsResult(listBrokersToolName, "get-broker-status", "list-queues"), nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
	)(next)

	res, err := wrapped(context.Background(), methodToolsList,
		listToolsRequest([]string{"Marketing"}))
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	assertNames(t, toolNames(t, res), []string{listBrokersToolName})
	assertRecord(t, assertOneRecord(t, buf), listReasonNotPermitted, "INFO")
}

// A verified token with no groups claim fails closed. WARN, because the token
// could not answer the authorization question at all — an IdP-side fault that
// affects every caller, unlike a caller who simply has no grants.
func TestWithListFiltering_MissingGroupsClaim_FailsClosedAndWarns(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	next, _ := listHandler(
		toolsResult(listBrokersToolName, "get-broker-status", "list-queues"), nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
	)(next)

	res, err := wrapped(context.Background(), methodToolsList, listToolsRequestMissingClaim())
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	assertNames(t, toolNames(t, res), []string{listBrokersToolName})
	assertRecord(t, assertOneRecord(t, buf), listReasonMissingClaim, "WARN")
}

// Nil Extra and nil TokenInfo are unreachable in production — the auth
// middleware 401s an unverified request long before this layer. Kept as an
// invariant assertion, not a live defence: unit tests construct requests
// directly, and a panic here would be a crash rather than a deny.
func TestWithListFiltering_NilExtraAndNilTokenInfo_FailClosedWithoutPanic(t *testing.T) {
	cases := []struct {
		name string
		req  *mcp.ServerRequest[*mcp.ListToolsParams]
	}{
		{
			name: "nil Extra",
			req:  &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}},
		},
		{
			name: "nil TokenInfo",
			req: &mcp.ServerRequest[*mcp.ListToolsParams]{
				Params: &mcp.ListToolsParams{},
				Extra:  &mcp.RequestExtra{},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, cleanup := captureSlog(t)
			defer cleanup()

			next, _ := listHandler(
				toolsResult(listBrokersToolName, "get-broker-status"), nil)
			wrapped := WithListFiltering(
				policyGranting(t, []string{"Ops"}, "get-broker-status"),
			)(next)

			res, err := wrapped(context.Background(), methodToolsList, tc.req)
			if err != nil {
				t.Fatalf("middleware returned error: %v", err)
			}

			assertNames(t, toolNames(t, res), []string{listBrokersToolName})
			assertRecord(t, assertOneRecord(t, buf), listReasonMissingClaim, "WARN")
		})
	}
}

// Grants covering every registered tool leave the list whole. The record still
// fires: an operator cannot infer "the filter ran and changed nothing" from
// silence.
func TestWithListFiltering_GrantsCoverEverything_ReturnsFullList(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	next, _ := listHandler(
		toolsResult(listBrokersToolName, "get-broker-status", "list-queues"), nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status", "list-queues"),
	)(next)

	res, err := wrapped(context.Background(), methodToolsList, listToolsRequest([]string{"Ops"}))
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	assertNames(t, toolNames(t, res),
		[]string{listBrokersToolName, "get-broker-status", "list-queues"})
	assertRecord(t, assertOneRecord(t, buf), listReasonUnfiltered, "INFO")
}

// Two callers, one middleware instance, different lists. The filter must be a
// function of the credential on THIS request, holding no per-connection state.
func TestWithListFiltering_TwoCallers_GetDifferentLists(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	policy := policyGranting(t, []string{"Ops"}, "get-broker-status")
	all := []string{listBrokersToolName, "get-broker-status", "list-queues"}

	nextOps, _ := listHandler(toolsResult(all...), nil)
	wrappedOps := WithListFiltering(policy)(nextOps)
	resOps, err := wrappedOps(context.Background(), methodToolsList,
		listToolsRequest([]string{"Ops"}))
	if err != nil {
		t.Fatalf("ops request errored: %v", err)
	}

	nextMkt, _ := listHandler(toolsResult(all...), nil)
	wrappedMkt := WithListFiltering(policy)(nextMkt)
	resMkt, err := wrappedMkt(context.Background(), methodToolsList,
		listToolsRequest([]string{"Marketing"}))
	if err != nil {
		t.Fatalf("marketing request errored: %v", err)
	}

	assertNames(t, toolNames(t, resOps), []string{listBrokersToolName, "get-broker-status"})
	assertNames(t, toolNames(t, resMkt), []string{listBrokersToolName})

	records := listFilterLogLines(t, buf)
	if len(records) != 2 {
		t.Fatalf("emitted %d records, want 2", len(records))
	}
	if records[0]["decision_reason"] != listReasonFiltered {
		t.Errorf("first record reason = %v, want %q",
			records[0]["decision_reason"], listReasonFiltered)
	}
	if records[1]["decision_reason"] != listReasonNotPermitted {
		t.Errorf("second record reason = %v, want %q",
			records[1]["decision_reason"], listReasonNotPermitted)
	}
}

// --- Silence is part of the contract ---

// Receiving middleware is process-wide: tools/call and every other method
// traverse this chain. They must pass through byte-identical AND silent — a
// broken method guard would otherwise flood the log on every ping.
func TestWithListFiltering_OtherMethods_PassThroughSilently(t *testing.T) {
	for _, method := range []string{"tools/call", "initialize", "ping", "notifications/initialized"} {
		t.Run(method, func(t *testing.T) {
			buf, cleanup := captureSlog(t)
			defer cleanup()

			sentinel := toolsResult(listBrokersToolName, "get-broker-status")
			next, calls := listHandler(sentinel, nil)
			wrapped := WithListFiltering(emptyPolicy(t))(next)

			res, err := wrapped(context.Background(), method, listToolsRequest([]string{"Ops"}))
			if err != nil {
				t.Fatalf("middleware returned error: %v", err)
			}
			if *calls != 1 {
				t.Fatalf("next called %d times, want 1", *calls)
			}
			// Pointer identity: not merely equal, but the same object,
			// so nothing was copied or rebuilt on the way out.
			if res != mcp.Result(sentinel) {
				t.Errorf("result was not passed through unchanged for %q", method)
			}
			if records := listFilterLogLines(t, buf); len(records) != 0 {
				t.Errorf("emitted %d records for %q, want 0", len(records), method)
			}
		})
	}
}

// An error from next short-circuits: no filtering is attempted on a result that
// may be nil, and no record is emitted for a list that never existed.
func TestWithListFiltering_NextErrors_PropagatesSilently(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	wantErr := errors.New("upstream failure")
	next, _ := listHandler(nil, wantErr)
	wrapped := WithListFiltering(emptyPolicy(t))(next)

	res, err := wrapped(context.Background(), methodToolsList, listToolsRequest([]string{"Ops"}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if res != nil {
		t.Errorf("res = %v, want nil", res)
	}
	if records := listFilterLogLines(t, buf); len(records) != 0 {
		t.Errorf("emitted %d records on error, want 0", len(records))
	}
}

// --- What the filter must not touch ---

// The ListToolsResult belongs to the SDK. Filtering returns a copy, so the
// input is observably unchanged and NextCursor survives.
func TestWithListFiltering_DoesNotMutateSDKResult(t *testing.T) {
	_, cleanup := captureSlog(t)
	defer cleanup()

	original := toolsResult(listBrokersToolName, "get-broker-status", "list-queues")
	original.NextCursor = "cursor-abc"
	originalCount := len(original.Tools)

	next, _ := listHandler(original, nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
	)(next)

	res, err := wrapped(context.Background(), methodToolsList, listToolsRequest([]string{"Ops"}))
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	if len(original.Tools) != originalCount {
		t.Errorf("input result was mutated: %d tools, want %d",
			len(original.Tools), originalCount)
	}
	if res == mcp.Result(original) {
		t.Error("middleware returned the SDK's own result rather than a copy")
	}

	filtered, ok := res.(*mcp.ListToolsResult)
	if !ok {
		t.Fatalf("result is %T, want *mcp.ListToolsResult", res)
	}
	// Pagination is inert today (default page size 1000 against ~23 tools),
	// but a filtered page must still resume correctly: the cursor is the last
	// UNFILTERED id and resumption is by id, so dropping it would silently
	// truncate a paginated read.
	if filtered.NextCursor != "cursor-abc" {
		t.Errorf("NextCursor = %q, want %q", filtered.NextCursor, "cursor-abc")
	}
}

// The caller's group names must never reach the log. authorization.go omits
// them deliberately on the deny path; this filter follows, so an audit trail
// cannot be mined for one caller's group membership.
func TestWithListFiltering_NeverLogsCallerGroups(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	const secretGroup = "Ops-Secret-Team-Alpha"
	next, _ := listHandler(
		toolsResult(listBrokersToolName, "get-broker-status"), nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{secretGroup}, "get-broker-status"),
	)(next)

	if _, err := wrapped(context.Background(), methodToolsList,
		listToolsRequest([]string{secretGroup})); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	if bytes.Contains(buf.Bytes(), []byte(secretGroup)) {
		t.Errorf("log contains the caller's group name %q:\n%s", secretGroup, buf.String())
	}
}

// A nil entry is a contract violation by the SDK, not an operator problem: it
// is reported at ERROR with a count, and the request still completes.
func TestWithListFiltering_NilTool_ReportsContractViolationAndContinues(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	res := toolsResult(listBrokersToolName, "get-broker-status")
	res.Tools = append(res.Tools, nil, nil)

	next, _ := listHandler(res, nil)
	wrapped := WithListFiltering(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
	)(next)

	got, err := wrapped(context.Background(), methodToolsList, listToolsRequest([]string{"Ops"}))
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	assertNames(t, toolNames(t, got), []string{listBrokersToolName, "get-broker-status"})

	errs := listFilterErrorLines(t, buf)
	if len(errs) != 1 {
		t.Fatalf("emitted %d contract-violation records, want 1", len(errs))
	}
	if got := errs[0]["level"]; got != "ERROR" {
		t.Errorf("level = %v, want ERROR", got)
	}
	if got := errs[0]["nil_tools"]; got != float64(2) {
		t.Errorf("nil_tools = %v, want 2", got)
	}
	// The normal per-request record still fires alongside it. It reads as
	// "filtered" rather than "unfiltered" because the nil entries counted
	// toward tools_before — the SDK said it was returning four tools and only
	// two came out. The counts on the ERROR record explain the difference.
	rec := assertOneRecord(t, buf)
	assertRecord(t, rec, listReasonFiltered, "INFO")
	if got := rec["tools_before"]; got != float64(4) {
		t.Errorf("tools_before = %v, want 4 (nil entries counted)", got)
	}
	if got := rec["tools_after"]; got != float64(2) {
		t.Errorf("tools_after = %v, want 2", got)
	}
}

// --- Composition-site invariant ---

// A nil policy means the install guard in main.go was dropped. Panic at wiring
// time rather than silently returning unfiltered lists forever.
func TestWithListFiltering_NilPolicy_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithListFiltering(nil) did not panic")
		}
	}()
	_ = WithListFiltering(nil)
}

// --- Outcome classification ---

// listFilterOutcome is pinned directly as well as through the middleware
// tests: it is the one place the reason vocabulary and the level are decided,
// and a wrong level is invisible in a passing behavioural test.
func TestListFilterOutcome(t *testing.T) {
	cases := []struct {
		name       string
		present    bool
		granted    int
		after      int
		before     int
		wantReason string
		wantLevel  slog.Level
	}{
		{
			name:       "missing claim outranks every other signal",
			present:    false,
			granted:    0,
			after:      1,
			before:     5,
			wantReason: listReasonMissingClaim,
			wantLevel:  slog.LevelWarn,
		},
		{
			name:       "groups present but nothing granted",
			present:    true,
			granted:    0,
			after:      1,
			before:     5,
			wantReason: listReasonNotPermitted,
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "grants cover every tool",
			present:    true,
			granted:    4,
			after:      5,
			before:     5,
			wantReason: listReasonUnfiltered,
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "partial filter",
			present:    true,
			granted:    2,
			after:      3,
			before:     5,
			wantReason: listReasonFiltered,
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "nothing granted on a server exposing only the exempt tool",
			present:    true,
			granted:    0,
			after:      1,
			before:     1,
			wantReason: listReasonNotPermitted,
			wantLevel:  slog.LevelInfo,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, level := listFilterOutcome(tc.present, tc.granted, tc.after, tc.before)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if level != tc.wantLevel {
				t.Errorf("level = %v, want %v", level, tc.wantLevel)
			}
		})
	}
}

// --- The invariant the design rests on ---

// Listed implies callable, and callable implies listed. Across every
// combination of tool and caller-group set, this filter's verdict must equal
// withAuthorization's — they read the same claim through requestGroups and
// decide through the same policy.Authorize.
//
// This is the test that catches a future refactor changing one path and not the
// other. It is the reason neither path reimplements the match.
func TestWithListFiltering_ListedImpliesCallable(t *testing.T) {
	_, cleanup := captureSlog(t)
	defer cleanup()

	// Every exempt tool is included, not just list-brokers. An earlier version
	// of this test hardcoded one name and so missed describe-semp-schema being
	// filtered out while remaining callable.
	allTools := []string{
		listBrokersToolName,
		describeSempSchemaToolName,
		"get-broker-status",
		"list-queues",
		"delete-queue",
		"get-redundancy-status",
	}

	policy := policyGranting(t, []string{"Ops"}, "get-broker-status", "list-queues")
	policy2 := policyGranting(t, []string{"Admin"}, "delete-queue")

	groupSets := [][]string{
		{"Ops"},
		{"Admin"},
		{"Ops", "Admin"},
		{"Marketing"},
		{},
		nil,
	}

	for _, policyUnderTest := range []*authz.Policy{policy, policy2} {
		for _, groups := range groupSets {
			next, _ := listHandler(toolsResult(allTools...), nil)
			wrapped := WithListFiltering(policyUnderTest)(next)

			res, err := wrapped(context.Background(), methodToolsList, listToolsRequest(groups))
			if err != nil {
				t.Fatalf("groups %v: middleware errored: %v", groups, err)
			}

			listed := make(map[string]bool)
			for _, name := range toolNames(t, res) {
				listed[name] = true
			}

			for _, tool := range allTools {
				// An exempt tool is registered without a policy, so it is
				// never gated on call and must never be filtered on list. Its
				// agreement is structural, not a policy verdict.
				if isExemptFromToolAuthorization(tool) {
					if !listed[tool] {
						t.Errorf("groups %v: exempt tool %q was filtered out", groups, tool)
					}
					continue
				}

				callable := callableUnderAuthorization(t, policyUnderTest, tool, groups)
				if listed[tool] != callable {
					t.Errorf("groups %v, tool %q: listed=%v but callable=%v",
						groups, tool, listed[tool], callable)
				}
			}
		}
	}
}

// isExemptFromToolAuthorization is the single point of truth for which tools
// bypass the policy, which means the invariant test above cannot catch the
// predicate itself being wrong — filter and test would agree while both were
// incorrect. That is not hypothetical: describe-semp-schema was missing from
// the filter's exemption for exactly this reason.
//
// This test closes that gap by pinning the predicate against the registration
// sites rather than against itself. Every tool registered outside the manager
// takes no policy argument and so is structurally exempt; if a future tool
// joins them in main.go, or one of these gains a policy, this fails.
func TestIsExemptFromToolAuthorization_MatchesUnpolicedRegistrations(t *testing.T) {
	// Registered directly on the server in main.go, each without a policy:
	// RegisterListBrokers and RegisterDescribeSempSchema.
	unpoliced := []string{listBrokersToolName, describeSempSchemaToolName}

	for _, name := range unpoliced {
		if !isExemptFromToolAuthorization(name) {
			t.Errorf("%q is registered without a policy but is not exempt — "+
				"tools/list would filter a tool that tools/call permits", name)
		}
	}

	// Anything registered through the manager IS policy-wrapped, so exempting
	// it would list a tool the call path denies.
	for _, name := range []string{"get-broker-status", "list-queues", "delete-queue"} {
		if isExemptFromToolAuthorization(name) {
			t.Errorf("%q is policy-wrapped but claims exemption", name)
		}
	}
}

// callableUnderAuthorization reports whether withAuthorization would dispatch
// this tool for this caller. Determined by running the real wrapper and
// observing whether next ran — not by re-asking the policy, which would test
// the policy against itself rather than the two wrappers against each other.
func callableUnderAuthorization(t *testing.T, policy *authz.Policy, tool string, groups []string) bool {
	t.Helper()
	rec := newRecordingHandler()
	wrapped := withAuthorization(policy, tool, "groups", rec.handler())
	if _, err := wrapped(context.Background(), requestWithGroups(groups)); err != nil {
		t.Fatalf("withAuthorization errored for %q: %v", tool, err)
	}
	return rec.calls == 1
}
