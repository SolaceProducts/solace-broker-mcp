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

package metrics

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

func newSecurityMetrics(t *testing.T) (*SecurityMetrics, *Provider) {
	t.Helper()
	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	sm, err := p.SecurityMetrics()
	if err != nil {
		t.Fatal(err)
	}
	return sm, p
}

// authFailureWant renders the whole mcp_auth_failure_total family with the
// given per-reason counts (missing keys are 0).
func authFailureWant(counts map[schema.AuthFailureReason]int) string {
	var b strings.Builder
	b.WriteString("# HELP mcp_auth_failure_total Number of credentials rejected at the HTTP boundary, by reason.\n")
	b.WriteString("# TYPE mcp_auth_failure_total counter\n")
	for _, reason := range schema.AuthFailureReasons() {
		fmt.Fprintf(&b, "mcp_auth_failure_total{reason=%q} %d\n", reason, counts[reason])
	}
	return b.String()
}

func TestSecurityMetrics_SeedsEveryAuthFailureReasonAtZero(t *testing.T) {
	_, p := newSecurityMetrics(t)

	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(authFailureWant(nil)), "mcp_auth_failure_total"); err != nil {
		t.Error(err)
	}
}

// Each reason lands on exactly its own series: after one record per reason,
// every series reads 1, none 0 or 2.
func TestSecurityMetrics_RecordAuthFailure_IncrementsOnlyThatReason(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	want := map[schema.AuthFailureReason]int{}
	for _, reason := range schema.AuthFailureReasons() {
		sm.RecordAuthFailure(context.Background(), string(reason))
		want[reason] = 1
	}

	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(authFailureWant(want)), "mcp_auth_failure_total"); err != nil {
		t.Error(err)
	}
}

func TestSecurityMetrics_AuthzDenied_AbsentUntilRecorded(t *testing.T) {
	_, p := newSecurityMetrics(t)

	mfs, err := p.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "mcp_authz_denied_total" {
			t.Fatalf("mcp_authz_denied_total present after registration with %d series; want absent until the first denial", len(mf.GetMetric()))
		}
	}
}

func TestSecurityMetrics_RecordAuthzDenied_LabelsToolAndReason(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	ctx := context.Background()

	sm.RecordAuthzDenied(ctx, "delete-queue", "not_permitted")
	sm.RecordAuthzDenied(ctx, "delete-queue", "not_permitted")
	sm.RecordAuthzDenied(ctx, "list-queues", "missing_claim")

	const want = `
# HELP mcp_authz_denied_total Number of tool calls refused by tool authorization, by tool and reason.
# TYPE mcp_authz_denied_total counter
mcp_authz_denied_total{reason="not_permitted",tool="delete-queue"} 2
mcp_authz_denied_total{reason="missing_claim",tool="list-queues"} 1
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(want), "mcp_authz_denied_total"); err != nil {
		t.Error(err)
	}
}

func TestSecurityMetrics_NilReceiver_NoOp(t *testing.T) {
	var sm *SecurityMetrics
	ctx := context.Background()
	sm.RecordAuthFailure(ctx, "expired")
	sm.RecordAuthzDenied(ctx, "delete-queue", "not_permitted")
}

func TestProvider_SecurityMetrics_RegistersOnce(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	again, err := p.SecurityMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if again != sm {
		t.Error("second SecurityMetrics() call returned a different recorder")
	}
}

// --- CountingAuthHook ---

type authFailureCall struct{ reason, sub, clientID string }

type recordingAuthHook struct {
	successes []*sdkauth.TokenInfo
	failures  []authFailureCall
}

func (r *recordingAuthHook) Success(_ context.Context, info *sdkauth.TokenInfo) {
	r.successes = append(r.successes, info)
}

func (r *recordingAuthHook) Failure(_ context.Context, reason, sub, clientID string) {
	r.failures = append(r.failures, authFailureCall{reason, sub, clientID})
}

func TestCountingAuthHook_NilMetrics_ReturnsNextUnchanged(t *testing.T) {
	next := &recordingAuthHook{}
	if got := CountingAuthHook(nil, next); got != next {
		t.Errorf("CountingAuthHook(nil, next) = %T, want next itself", got)
	}
}

// The counter's reason and the delegated reason are the same string from the
// same call, for every value in the vocabulary; sub/clientID pass through.
func TestCountingAuthHook_Failure_CountsAndDelegatesSameReason(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	next := &recordingAuthHook{}
	hook := CountingAuthHook(sm, next)

	var wantCalls []authFailureCall
	want := map[schema.AuthFailureReason]int{}
	for _, reason := range schema.AuthFailureReasons() {
		hook.Failure(context.Background(), string(reason), "alice", "agent-1")
		wantCalls = append(wantCalls, authFailureCall{reason: string(reason), sub: "alice", clientID: "agent-1"})
		want[reason] = 1
	}

	if !reflect.DeepEqual(next.failures, wantCalls) {
		t.Errorf("delegated Failure calls = %+v, want %+v", next.failures, wantCalls)
	}
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(authFailureWant(want)), "mcp_auth_failure_total"); err != nil {
		t.Error(err)
	}
}

func TestCountingAuthHook_Success_DelegatesWithoutCounting(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	next := &recordingAuthHook{}
	hook := CountingAuthHook(sm, next)
	info := &sdkauth.TokenInfo{UserID: "alice"}

	hook.Success(context.Background(), info)

	if len(next.successes) != 1 || next.successes[0] != info {
		t.Errorf("delegated Success calls = %v, want exactly the one TokenInfo passed in", next.successes)
	}
	if len(next.failures) != 0 {
		t.Errorf("Success delegated %d Failure calls, want 0", len(next.failures))
	}
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(authFailureWant(nil)), "mcp_auth_failure_total"); err != nil {
		t.Error(err)
	}
}
