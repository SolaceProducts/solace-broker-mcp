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

// A write blocked by SOL-153080's owner check increments
// mcp_owner_validation_total (SOL-154679). Before this, a blocked call was only
// outcome=error, error_type=execution_error on mcp_tool_invocation_total —
// indistinguishable from any other execution failure — so the rejection rate
// the case for a broker-side fix rests on was not measurable.
//
// The counter is recorded in CallTool, next to mcp_broker_authz_denied_total,
// not inside ownerValidatingHandler: CallTool has the broker alias (the
// handler's params have "broker" stripped), and classifying there keeps this
// next to the other error-type decisions.
package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

const ownerValidationFamily = "mcp_owner_validation_total"

// ownerMetricManager registers one stub tool (destructive as asked) whose Handle
// returns handleErr, on a manager wired to a real metrics provider.
func ownerMetricManager(t *testing.T, name string, destructive bool, handleErr error) (*ToolManager, *metrics.Provider) {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewToolManager(newTestPool(t), WithToolMetrics(tm))
	h := newStubHandler(name)
	h.annotations = Annotations{Destructive: &destructive}
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handleErr
	}
	mgr.Register(h)
	return mgr, p
}

func callOwnerMetricTool(t *testing.T, mgr *ToolManager, tool string) {
	t.Helper()
	if _, err := mgr.CallTool(context.Background(), tool, map[string]any{"broker": "dev", "msgVpnName": "default"}, Identity{}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
}

// ownerValidationLines returns the scraped sample lines of the family, so a
// test can assert on the whole series set rather than one substring.
func ownerValidationLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, ownerValidationFamily+"{") {
			out = append(out, l)
		}
	}
	return out
}

func TestOwnerValidationMetric_ConfirmedAbsentOwner_CountsRejectedNotFound(t *testing.T) {
	for _, tc := range []struct {
		tool        string
		destructive bool
	}{
		{"create-queue", false},
		{"update-queue", true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			mgr, p := ownerMetricManager(t, tc.tool, tc.destructive,
				&ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"})
			callOwnerMetricTool(t, mgr, tc.tool)

			body := scrape(t, p)
			want := fmt.Sprintf(`%s{broker="dev",outcome="rejected_not_found",tool=%q} 1`, ownerValidationFamily, tc.tool)
			if got := ownerValidationLines(body); len(got) != 1 || got[0] != want {
				t.Errorf("series = %v, want exactly [%s]", got, want)
			}
			// The tool-RED classification is unchanged: this counter adds a
			// distinguishing signal, it does not re-label the invocation.
			if wantInv := fmt.Sprintf(`mcp_tool_invocation_total{broker="dev",error_type="execution_error",outcome="error",tool=%q} 1`, tc.tool); !strings.Contains(body, wantInv) {
				t.Errorf("scrape missing %q — the invocation metric's classification changed", wantInv)
			}
		})
	}
}

func TestOwnerValidationMetric_InconclusiveCheck_CountsRejectedCheckFailed(t *testing.T) {
	mgr, p := ownerMetricManager(t, "create-queue", false, &ownerCheckFailedError{
		cause: &sempv2.SEMPError{StatusCode: 503}, objectKind: "queue", owner: "real-user", msgVpn: "default",
		stage: ownerCheckStageClientUsername,
	})
	callOwnerMetricTool(t, mgr, "create-queue")

	want := ownerValidationFamily + `{broker="dev",outcome="rejected_check_failed",tool="create-queue"} 1`
	if got := ownerValidationLines(scrape(t, p)); len(got) != 1 || got[0] != want {
		t.Errorf("series = %v, want exactly [%s]", got, want)
	}
}

// TestOwnerValidationMetric_CheckFailedWrappingCode72_StillCounts is the
// ordering case: an ownerCheckFailedError whose cause is a SEMP code-72 denial
// also matches isBrokerAuthzDenial through Unwrap, and that branch returns
// early. The write was blocked either way, so the owner counter must run ahead
// of that return — and the hop-2 denial must still be recorded as before.
func TestOwnerValidationMetric_CheckFailedWrappingCode72_StillCounts(t *testing.T) {
	mgr, p := ownerMetricManager(t, "create-queue", false, &ownerCheckFailedError{
		cause: &sempv2.SEMPError{StatusCode: 403, SEMPCode: sempv2.SEMPCodePermissionDenied}, objectKind: "queue",
		owner: "real-user", msgVpn: "default", stage: ownerCheckStageClientUsername,
	})
	callOwnerMetricTool(t, mgr, "create-queue")

	body := scrape(t, p)
	if want := ownerValidationFamily + `{broker="dev",outcome="rejected_check_failed",tool="create-queue"} 1`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q — the early return for a hop-2 denial skipped the owner counter", want)
	}
	if want := `mcp_broker_authz_denied_total{broker="dev",reason="permission_denied",tool="create-queue"} 1`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q — recording the owner counter displaced the hop-2 denial", want)
	}
	if want := `error_type="broker_permission_denied"`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q — the hop-2 error_type classification changed", want)
	}
}

func TestOwnerValidationMetric_UnrelatedFailureAndSuccess_AreNotCounted(t *testing.T) {
	for name, handleErr := range map[string]error{
		"unrelated error": errors.New("broker refused"),
		"success":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			mgr, p := ownerMetricManager(t, "create-queue", false, handleErr)
			callOwnerMetricTool(t, mgr, "create-queue")
			if got := ownerValidationLines(scrape(t, p)); len(got) != 0 {
				t.Errorf("owner counter has series %v for a call the owner check did not block", got)
			}
		})
	}
}

func TestOwnerValidationMetric_MetricsDisabled_IsNoop(t *testing.T) {
	mgr := NewToolManager(newTestPool(t)) // no WithToolMetrics: m.metrics is nil
	h := newStubHandler("create-queue")
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, &ownerNotFoundError{owner: "ghost", msgVpn: "default", objectKind: "queue"}
	}
	mgr.Register(h)
	callOwnerMetricTool(t, mgr, "create-queue") // must not panic
}

// TestOwnerValidationMetric_RealValidator_EndToEnd drives the real
// ownerValidatingHandler through NewToolManagerFromComposite and CallTool
// against a stub SEMP server, so the counter is pinned to the actual rejection
// rather than to a hand-built error the validator might not produce.
func TestOwnerValidationMetric_RealValidator_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			fmt.Fprint(w, `{"data":{},"meta":{"responseCode":200}}`)
		case strings.Contains(r.URL.Path, "clientUsernames/real-user"):
			fmt.Fprint(w, `{"data":{"clientUsername":"real-user"},"meta":{"responseCode":200}}`)
		case strings.Contains(r.URL.Path, "clientUsernames"):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"meta":{"responseCode":400,"error":{"code":6,"description":"not found","status":"NOT_FOUND"}}}`)
		default:
			fmt.Fprint(w, `{"data":{"msgVpnName":"default"},"meta":{"responseCode":200}}`)
		}
	}))
	t.Cleanup(srv.Close)

	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pool := semp.NewBrokerPool(writeTestConfig(t, [2]string{"dev", srv.URL}), nil)
	mgr := NewToolManagerFromComposite(pool, realTools, composite.NewCompositeExecutor(ops), WithToolMetrics(tm))

	call := func(owner string) {
		t.Helper()
		if _, err := mgr.CallTool(context.Background(), "create-queue", map[string]any{
			"broker": "dev", "msgVpnName": "default", "queueName": "q1",
			"queueConfig": map[string]any{"owner": owner},
		}, Identity{}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}
	call("real-user") // passes the check: must not be counted
	call("ghost")     // confirmed absent

	want := ownerValidationFamily + `{broker="dev",outcome="rejected_not_found",tool="create-queue"} 1`
	if got := ownerValidationLines(scrape(t, p)); len(got) != 1 || got[0] != want {
		t.Errorf("series = %v, want exactly [%s]", got, want)
	}
}
