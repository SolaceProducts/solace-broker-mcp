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

// mcp_authz_denied_total{tool,reason} (SOL-152099). The audit half of the same
// signal is authz_denied_audit_test.go.

package tools

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

func newSecurityMetrics(t *testing.T) (*metrics.SecurityMetrics, *metrics.Provider) {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sm, err := p.SecurityMetrics()
	if err != nil {
		t.Fatal(err)
	}
	return sm, p
}

func scrape(t *testing.T, p *metrics.Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// Labels render alphabetically, so reason precedes tool.
func authzDeniedLine(reason, tool string, n int) string {
	return fmt.Sprintf("mcp_authz_denied_total{reason=%q,tool=%q} %d", reason, tool, n)
}

// assertOnlyAuthzDenied checks the family holds exactly the one expected series.
func assertOnlyAuthzDenied(t *testing.T, body, reason, tool string) {
	t.Helper()
	if want := authzDeniedLine(reason, tool, 1); !strings.Contains(body, want) {
		t.Errorf("scrape missing %q", want)
	}
	if n := strings.Count(body, "mcp_authz_denied_total{"); n != 1 {
		t.Errorf("mcp_authz_denied_total has %d series, want 1", n)
	}
}

func TestAuthzDenied_MissingClaim_IncrementsCounter(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "delete-queue"),
		"delete-queue", "groups", false, sm, newRecordingHandler().handler())

	_, _ = wrapped(seeded(requestMissingGroupsClaim()))

	assertOnlyAuthzDenied(t, scrape(t, p), decisionReasonMissingClaim, "delete-queue")
}

func TestAuthzDenied_NotPermitted_IncrementsCounter(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	wrapped := withAuthorization(emptyPolicy(t), "delete-queue", "groups", false, sm, newRecordingHandler().handler())

	_, _ = wrapped(seeded(requestWithGroups([]string{"Contractors"})))

	assertOnlyAuthzDenied(t, scrape(t, p), decisionReasonNotPermitted, "delete-queue")
}

func TestAuthzDenied_Allow_RecordsNothing(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	rec := newRecordingHandler()
	wrapped := withAuthorization(policyGranting(t, []string{"Ops"}, "delete-queue"), "delete-queue", "groups", false, sm, rec.handler())

	_, _ = wrapped(seeded(requestWithGroups([]string{"Ops"})))

	if rec.calls != 1 {
		t.Fatalf("next called %d times, want 1", rec.calls)
	}
	if body := scrape(t, p); strings.Contains(body, "mcp_authz_denied_total") {
		t.Error("mcp_authz_denied_total present after an allow, want absent")
	}
}

// On each deny branch the counter's labels equal the authz_denied audit
// record's reason and tool, since both come from the same decision. next is a
// real CallTool dispatch, as in authz_denied_audit_test.go.
func TestAuthzDenied_CounterAndAuditRecordAgreeOnReason(t *testing.T) {
	cases := []struct {
		name       string
		policy     func(t *testing.T) *authz.Policy
		req        *mcp.CallToolRequest
		wantReason string
	}{
		{
			name:       "missing_claim",
			policy:     func(t *testing.T) *authz.Policy { return policyGranting(t, []string{"Ops"}, "delete-queue") },
			req:        requestMissingGroupsClaim(),
			wantReason: decisionReasonMissingClaim,
		},
		{
			name:       "not_permitted",
			policy:     emptyPolicy,
			req:        requestWithGroups([]string{"Contractors"}),
			wantReason: decisionReasonNotPermitted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm, p := newSecurityMetrics(t)
			mgr := auditTestManager(t, true)
			wrapped := withAuthorization(tc.policy(t), "delete-queue", "groups", true, sm, callToolNext(mgr))
			ctx := ctxWithPrincipal(context.Background(), tc.req)

			records := captureAudit(t, slog.LevelInfo, func() {
				if _, err := wrapped(ctx, tc.req); err != nil {
					t.Fatalf("wrapper returned error: %v", err)
				}
			})

			denies := ofType(records, audit.EventAuthzDenied)
			if len(denies) != 1 {
				t.Fatalf("want exactly 1 authz_denied record, got %d:\n%v", len(denies), records)
			}
			reason, _ := denies[0]["reason"].(string)
			tool, _ := denies[0]["tool"].(string)
			if reason != tc.wantReason {
				t.Errorf("audit reason = %q, want %q", reason, tc.wantReason)
			}
			assertOnlyAuthzDenied(t, scrape(t, p), reason, tool)
		})
	}
}

// RegisterWithServer must hand mgr.securityMetrics to the wrapper; dropping
// the argument would compile (nil is valid) and silently turn the counter off.
// The in-memory client carries no TokenInfo, so the denial is missing_claim.
func TestRegisterWithServer_PassesSecurityMetricsToAuthorization(t *testing.T) {
	sm, p := newSecurityMetrics(t)
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool, WithSecurityMetrics(sm))
	mgr.Register(newStubHandler("test-tool"))
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, emptyPolicy(t), "groups")

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "test-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("call was not denied; the test needs a denial to observe the counter")
	}

	assertOnlyAuthzDenied(t, scrape(t, p), decisionReasonMissingClaim, "test-tool")
}
