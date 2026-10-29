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

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SOL-156509: a client can cancel a tool call two ways, and for a write the
// change may already have reached the broker when it does. These tests drive
// a real destructive composite through the real SEMPv2 client and sender
// against a fake broker that records what it received, for each row of the
// ticket's table and over both cancellation paths:
//
//   - notifications/cancelled: a stateful server; the go-sdk client sends it
//     when the call's context is cancelled (mcp/transport.go cancelCall).
//   - a dropped request: a stateless server with PropagateRequestCancellation,
//     as cmd/server's newMCPStreamableOptions(true) builds it.
//
// Each case asserts the operation audit record (outcome, error_type,
// request_sent), the fake broker's log (the "broker state"), and that the
// metric label, the `tool invoked` line and the dispatch span agree with the
// audit record on outcome and error_type.

// cancelWait bounds every wait here, so a regression shows as a failure, not
// a hang.
const cancelWait = 5 * time.Second

// recordingBroker is a fake SEMP broker. Every request it receives is logged.
// In hold mode it reads the request, reports it on received, and never
// answers — a write the broker has but whose reply has not arrived. With
// bodyStatus set it answers with that status and the start of a body, then
// holds the rest — a reply whose body is still being read.
type recordingBroker struct {
	*httptest.Server
	hold       bool
	bodyStatus int
	received   chan struct{}

	mu   sync.Mutex
	seen []string
}

func newRecordingBroker(t *testing.T, hold bool, bodyStatus int) *recordingBroker {
	t.Helper()
	b := &recordingBroker{hold: hold, bodyStatus: bodyStatus, received: make(chan struct{}, 8)}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.seen = append(b.seen, r.Method+" "+r.URL.Path)
		b.mu.Unlock()
		if b.bodyStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(b.bodyStatus)
			_, _ = w.Write([]byte(`{"meta":`))
			w.(http.Flusher).Flush()
			b.received <- struct{}{}
			<-r.Context().Done()
			return
		}
		b.received <- struct{}{}
		if b.hold {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"meta":{"responseCode":200}}`))
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *recordingBroker) requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// writeProbeHandler is a destructive one-write composite run through the real
// CompositeExecutor and SEMPv2 client, so the request goes through
// resilience.Sender exactly as a production tool's does.
type writeProbeHandler struct {
	method string
	// waitForCancel blocks the handler until its context ends BEFORE it
	// issues the write: the cancel lands before anything is sent.
	waitForCancel bool
	// holdAfterSuccess blocks the handler after the broker answered until its
	// context ends: the success arrives before the cancel.
	holdAfterSuccess bool
	entered          chan struct{}
	succeeded        chan struct{} // closed once the broker's success is in hand
}

func (h *writeProbeHandler) Metadata() tools.Metadata {
	yes := true
	return tools.Metadata{
		Name:        "write-probe-tool",
		Description: "issues one SEMP write",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msgVpnName": map[string]any{"type": "string"}},
			"required":   []string{"msgVpnName"},
		},
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  tools.Annotations{Destructive: &yes},
	}
}

func (h *writeProbeHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	close(h.entered)
	if h.waitForCancel {
		<-ctx.Done()
	}
	ops := map[string]*sempv2.Operation{
		"config/writeQueue": {ID: "writeQueue", Method: h.method, Path: "/SEMP/v2/config/msgVpns/default/queues/q1"},
	}
	tool := composite.CompositeTool{
		Name:   "write-probe-tool",
		Steps:  []composite.Step{{ID: "write", Operation: "config/writeQueue"}},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
	out, err := composite.NewCompositeExecutor(ops).Execute(ctx, tool, tc.SEMPv2Client, params)
	if err != nil {
		return nil, err
	}
	close(h.succeeded)
	if h.holdAfterSuccess {
		<-ctx.Done()
	}
	return &tools.ToolResult{StructuredContent: out}, nil
}

// writePoolFor points "dev" at brokerURL. sempYAML, when set, is the
// top-level semp: block (used to shorten the request timeout).
func writePoolFor(t *testing.T, brokerURL, sempYAML string) *semp.BrokerPool {
	t.Helper()
	cfgYAML := "mcp_client_auth:\n  mode: disabled\n" + sempYAML + "brokers:\n" +
		"  dev:\n    url: " + brokerURL + "\n    auth:\n      mode: basic\n" +
		"      username: admin\n      password: admin\n"
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	t.Cleanup(pool.Close)
	return pool
}

// writeSession serves h over the streamable transport, stateful or stateless
// the way cmd/server's newMCPStreamableOptions configures it, and connects.
func writeSession(t *testing.T, h tools.ToolHandler, pool *semp.BrokerPool, stateless bool, tm *metrics.ToolMetrics) *mcp.ClientSession {
	t.Helper()
	mgr := tools.NewToolManagerFromComposite(pool, nil, nil, tools.WithToolMetrics(tm), tools.WithAuditLog(true))
	mgr.Register(h)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")

	var handler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: stateless, PropagateRequestCancellation: stateless})
	handler = correlation.Middleware(handler)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: stateless}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if stateless {
		if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
			t.Fatalf("negotiated %q, want 2026-07-28; the dropped-request path would not be exercised", got)
		}
	}
	return session
}

// awaitDispatchSpan waits for CallTool's span to end. It is the last thing
// CallTool's defers do, so once it has ended the log line, the metric and the
// audit record are all written — the client may have returned long before.
func awaitDispatchSpan(t *testing.T, sr interface{ Ended() []sdktrace.ReadOnlySpan }) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(cancelWait)
	for time.Now().Before(deadline) {
		for _, s := range sr.Ended() {
			if s.Name() == "tools.CallTool" {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tools.CallTool span never ended; the call did not finish server-side")
	return nil
}

type writeCase struct {
	name             string
	method           string
	hold             bool   // broker holds the reply
	bodyStatus       int    // broker sends this status and part of a body, then holds the rest
	brokerDown       bool   // nothing listens at the broker URL
	sempYAML         string // top-level semp: block
	waitForCancel    bool
	holdAfterSuccess bool
	// cancelOn picks the moment the client cancels: "" never, "entered" once
	// the handler runs, "received" once the broker has the request,
	// "succeeded" once the handler holds the broker's success reply. Not
	// "received" for the success case: a dropped request cancels the handler
	// fast enough to land while the reply is still being read, which is a
	// cancel after send, not a success.
	cancelOn      string
	wantOutcome   string
	wantErrorType string
	wantSent      *bool // nil: request_sent must be absent
	wantRequests  int   // requests the broker received
}

func TestCancelledWrite_OutcomeAndRequestSent(t *testing.T) {
	yes, no := true, false
	shortTimeout := "semp:\n  request_timeout_duration: 300ms\n  retries: 0\n"
	cases := []writeCase{
		{name: "cancelled before send", method: http.MethodPost, waitForCancel: true, cancelOn: "entered",
			wantOutcome: "cancelled", wantSent: &no, wantRequests: 0},
		{name: "cancelled after send, POST", method: http.MethodPost, hold: true, cancelOn: "received",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
		{name: "cancelled after send, PATCH", method: http.MethodPatch, hold: true, cancelOn: "received",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
		{name: "cancelled after send, DELETE", method: http.MethodDelete, hold: true, cancelOn: "received",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
		{name: "timed out after send", method: http.MethodPost, hold: true, sempYAML: shortTimeout,
			wantOutcome: "error", wantErrorType: "execution_error", wantSent: &yes, wantRequests: 1},
		{name: "failed before send", method: http.MethodPost, brokerDown: true,
			wantOutcome: "error", wantErrorType: "execution_error", wantSent: &no, wantRequests: 0},
		{name: "broker success before the cancel", method: http.MethodDelete, holdAfterSuccess: true, cancelOn: "succeeded",
			wantOutcome: "success", wantSent: nil, wantRequests: 1},
		// The cancel lands in sempv2.Client's response-body read, not in
		// Sender.Do. Still cancelled: the body-read error wraps the ctx error.
		{name: "cancelled while reading the reply, POST", method: http.MethodPost, bodyStatus: http.StatusOK, cancelOn: "reading",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
		{name: "cancelled while reading the reply, DELETE", method: http.MethodDelete, bodyStatus: http.StatusOK, cancelOn: "reading",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
		// The broker's 403 status arrived but its reason had not: the call is
		// cancelled with request_sent=true, the "check the broker" answer,
		// rather than an error built from half a reply.
		{name: "cancelled while reading an error reply", method: http.MethodPost, bodyStatus: http.StatusForbidden, cancelOn: "reading",
			wantOutcome: "cancelled", wantSent: &yes, wantRequests: 1},
	}
	for _, path := range []struct {
		name      string
		stateless bool
	}{
		{"notifications/cancelled", false},
		{"dropped request", true},
	} {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				runWriteCase(t, tc, path.stateless)
			})
		}
	}
}

func runWriteCase(t *testing.T, tc writeCase, stateless bool) {
	sr := recordRequestPathSpans(t)
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	logged := captureLogRecords(t)

	broker := newRecordingBroker(t, tc.hold, tc.bodyStatus)
	brokerURL := broker.URL
	if tc.brokerDown {
		broker.Close()
	}
	h := &writeProbeHandler{method: tc.method, waitForCancel: tc.waitForCancel,
		holdAfterSuccess: tc.holdAfterSuccess, entered: make(chan struct{}), succeeded: make(chan struct{})}
	session := writeSession(t, h, writePoolFor(t, brokerURL, tc.sempYAML), stateless, tm)

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = session.CallTool(callCtx, &mcp.CallToolParams{
			Name:      "write-probe-tool",
			Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
		})
	}()

	switch tc.cancelOn {
	case "entered":
		waitFor(t, h.entered, "the handler to start")
		cancel()
	case "received":
		waitFor(t, broker.received, "the broker to receive the write")
		cancel()
	case "succeeded":
		waitFor(t, h.succeeded, "the broker's success to reach the handler")
		cancel()
	case "reading":
		// The broker has flushed the status line and part of the body. Give
		// the client a moment to parse the headers and start the body read,
		// so the cancel lands there. Either order ends cancelled with
		// request_sent=true, so this delay cannot make the test flaky; it
		// only decides which read the cancel interrupts.
		waitFor(t, broker.received, "the broker to send its reply headers")
		time.Sleep(100 * time.Millisecond)
		cancel()
	}
	dispatch := awaitDispatchSpan(t, sr)
	waitFor(t, done, "the client call to return")

	// Broker state: what actually reached it.
	if got := broker.requests(); len(got) != tc.wantRequests {
		t.Errorf("broker received %d request(s) %v, want %d", len(got), got, tc.wantRequests)
	}

	// The audit record, pinned against the ticket's table.
	record := oneOperationAuditRecord(t, logged)
	if got := stringField(record, "outcome"); got != tc.wantOutcome {
		t.Errorf("audit outcome = %q, want %q", got, tc.wantOutcome)
	}
	if got := stringField(record, "error_type"); got != tc.wantErrorType {
		t.Errorf("audit error_type = %q, want %q", got, tc.wantErrorType)
	}
	sent, hasSent := record["request_sent"]
	switch {
	case tc.wantSent == nil && hasSent:
		t.Errorf("audit request_sent = %v, want absent on success", sent)
	case tc.wantSent != nil && (!hasSent || sent != *tc.wantSent):
		t.Errorf("audit request_sent = %v (present=%v), want %v", sent, hasSent, *tc.wantSent)
	}

	// The other three signals agree with it.
	series := toolMetricSeries(t, p)
	if len(series) != 1 {
		t.Fatalf("mcp_tool_invocation_total series = %d, want 1: %v", len(series), series)
	}
	line := oneRecordWithMsg(t, logged, "tool invoked")
	// The log line carries request_sent on a cancelled call too, so the answer
	// survives with the audit log off (its default) and for tools the audit
	// record does not cover.
	if tc.wantOutcome == "cancelled" {
		if got, ok := line["request_sent"]; !ok || got != *tc.wantSent {
			t.Errorf("log line request_sent = %v (present=%v), want %v", got, ok, *tc.wantSent)
		}
	}
	for _, key := range []string{"outcome", "error_type"} {
		want := stringField(record, key)
		spanValue, _ := spanAttr(dispatch, key)
		for surface, got := range map[string]string{
			"metric label": series[0][key],
			"log line":     stringField(line, key),
			"span":         spanValue,
		} {
			if got != want {
				t.Errorf("%s %s = %q, audit record = %q", surface, key, got, want)
			}
		}
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(cancelWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}
