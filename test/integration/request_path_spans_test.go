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

// Spans at every layer of the request path (SOL-152421).
//
// Integration rather than unit tests because the property under test is a
// RELATIONSHIP between layers that each own their own span. Every layer's span
// can be correct in isolation while the chain is broken: a layer threading
// context.Background() produces a span that looks right and silently detaches
// the whole subtree below it. Only reading back parent/child edges from one
// real call catches that — and it is the same failure Story 47's exemplars
// (SOL-152419) depend on not happening.
package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/tracing"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// spanForwarder lets each test swap in its own recorder without calling
// otel.SetTracerProvider more than once.
//
// Not optional: every package this story instruments holds its tracer in a
// package-scoped var resolved at package-init time, and only the FIRST
// SetTracerProvider call is honored for handles obtained before it. A per-test
// "install a fresh provider" helper captures spans for the first test in the
// binary and silently nothing afterwards. internal/tokenexchange/span_test.go
// documents the same trap at length, having hit it first.
type spanForwarder struct {
	mu   sync.Mutex
	next sdktrace.SpanProcessor
}

func (p *spanForwarder) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.mu.Lock()
	next := p.next
	p.mu.Unlock()
	if next != nil {
		next.OnStart(ctx, s)
	}
}

func (p *spanForwarder) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	next := p.next
	p.mu.Unlock()
	if next != nil {
		next.OnEnd(s)
	}
}

func (p *spanForwarder) Shutdown(context.Context) error   { return nil }
func (p *spanForwarder) ForceFlush(context.Context) error { return nil }

var (
	pathSpanForwarder = &spanForwarder{}
	installPathTracer sync.Once
)

// recordRequestPathSpans installs the shared always-sampling provider once and
// returns a recorder scoped to this test. Not parallel-safe with another caller
// (the recorder swap races its assertions), so nothing here calls t.Parallel.
func recordRequestPathSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installPathTracer.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(pathSpanForwarder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		))
		// Production installs this in tracing.New, which this binary never
		// calls; without it the traceparent cases below prove nothing.
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})

	sr := tracetest.NewSpanRecorder()
	pathSpanForwarder.mu.Lock()
	pathSpanForwarder.next = sr
	pathSpanForwarder.mu.Unlock()
	t.Cleanup(func() {
		pathSpanForwarder.mu.Lock()
		pathSpanForwarder.next = nil
		pathSpanForwarder.mu.Unlock()
	})
	return sr
}

// fakeBroker serves one minimal SEMPv2 response, so the client completes a real
// request and its span records a real outcome.
func fakeBroker(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"msgVpnName":"default"},"meta":{"responseCode":200}}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// spanPoolFor points the "dev" alias at brokerURL, so the SEMP client reaches
// fakeBroker rather than a dead port.
//
// A second alias, "Dev-EU", is configured in mixed case on purpose. Broker
// lookup is case-insensitive while DisplayName preserves the configured
// casing, so a caller typing "dev-eu" exercises real normalisation: every
// canonicalized surface must report "Dev-EU". Without an alias whose
// configured form differs from what a caller types, a broker-key comparison
// passes trivially and verifies nothing — "dev" is spelled the same either
// way.
func spanPoolFor(t *testing.T, brokerURL string) *semp.BrokerPool {
	t.Helper()
	cfgYAML := "mcp_client_auth:\n  mode: disabled\nbrokers:\n" +
		"  dev:\n    url: " + brokerURL + "\n    auth:\n      mode: basic\n" +
		"      username: admin\n      password: admin\n" +
		"  Dev-EU:\n    url: " + brokerURL + "\n    auth:\n      mode: basic\n" +
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

// compositeStubHandler runs a REAL CompositeExecutor against the REAL SEMPv2
// client it is handed, so the composite.Execute -> semp.request span pair comes
// off the production code path. Hand-made spans would prove nothing about
// whether those layers actually thread the request context.
type compositeStubHandler struct {
	failStep bool
	panics   bool
}

func (h *compositeStubHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name:        "span-probe-tool",
		Description: "exercises the request path",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msgVpnName": map[string]any{"type": "string"}},
			"required":   []string{"msgVpnName"},
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		Annotations: tools.Annotations{ReadOnly: true},
	}
}

func (h *compositeStubHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	if h.panics {
		panic("span-probe-tool deliberate panic")
	}
	path := "/SEMP/v2/monitor/msgVpns/default"
	if h.failStep {
		// An unresolvable path placeholder: buildURL rejects it before any
		// HTTP call, which is a real error path through Execute.
		path = "/SEMP/v2/monitor/msgVpns/{missingParam}"
	}
	ops := map[string]*sempv2.Operation{
		"monitor/getMsgVpn": {ID: "getMsgVpn", Method: http.MethodGet, Path: path},
	}
	tool := composite.CompositeTool{
		Name:   "span-probe-tool",
		Steps:  []composite.Step{{ID: "step1", Operation: "monitor/getMsgVpn"}},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
	out, err := composite.NewCompositeExecutor(ops).Execute(ctx, tool, tc.SEMPv2Client, params)
	if err != nil {
		return nil, err
	}
	return &tools.ToolResult{StructuredContent: out}, nil
}

// tracedSession assembles the production /mcp layer order — correlation
// outside, then tracing, then the MCP handler — and returns a connected
// session.
func tracedSession(t *testing.T, h tools.ToolHandler, brokerURL string, tracingEnabled bool) *mcp.ClientSession {
	t.Helper()
	return tracedSessionWith(t, h, brokerURL, tracingEnabled, nil)
}

// tracedSessionWith is tracedSession with a metrics recorder wired into the
// manager, for the cross-signal test that has to read spans and metrics
// produced by the same call. nil tm is the disabled path the other tests use.
//
// extraOpts is variadic rather than a fifth positional parameter so the
// existing call sites, none of which want one, stay untouched. Today only
// tools.WithAuditLog uses it (SOL-154036).
func tracedSessionWith(t *testing.T, h tools.ToolHandler, brokerURL string, tracingEnabled bool, tm *metrics.ToolMetrics, extraOpts ...tools.ManagerOption) *mcp.ClientSession {
	t.Helper()
	pool := spanPoolFor(t, brokerURL)
	// tools.WithToolMetrics rather than a positional argument: the constructor
	// moved to variadic ManagerOptions in SOL-152090. A nil tm would be a nil
	// option, which the constructor treats as a no-op, so it is omitted
	// instead.
	var opts []tools.ManagerOption
	if tm != nil {
		opts = append(opts, tools.WithToolMetrics(tm))
	}
	opts = append(opts, extraOpts...)
	mgr := tools.NewToolManagerFromComposite(pool, nil, nil, opts...)
	mgr.Register(h)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")
	// Registered separately in production too (it takes no broker parameter),
	// and separately here because it is one of the dispatch sites that bypass
	// ToolManager.CallTool — see TestRequestPathSpans_BypassDispatchSitesSpan.
	tools.RegisterListBrokers(server, pool, tm)

	var handler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil)
	// Mirrors buildMCPEndpoint: tracing INSIDE correlation, so the entry span
	// can stamp an ID that already exists.
	if tracingEnabled {
		handler = tracing.HTTPMiddleware("/mcp", handler)
	}
	handler = correlation.Middleware(handler)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callProbeTool(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "span-probe-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
}

// oneSpan returns the single ended span with the given name.
func oneSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d spans named %q, want exactly 1; ended spans: %s",
			len(found), name, spanNames(sr))
	}
	return found[0]
}

// entrySpanParenting returns the "POST /mcp" span that is child's parent.
// Deliberately does not assert only one entry span exists: the MCP client
// issues several POSTs per session (initialize, initialized, the call,
// teardown), each correctly getting its own. Anchoring on the edge finds the
// POST that carried the tool call, and is a stronger assertion than a name.
func entrySpanParenting(t *testing.T, sr *tracetest.SpanRecorder, child sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	want := child.Parent().SpanID()
	for _, s := range sr.Ended() {
		if s.Name() == "POST /mcp" && s.SpanContext().SpanID() == want {
			return s
		}
	}
	t.Fatalf("no \"POST /mcp\" span with span ID %v (the parent of %q); ended spans: %s",
		want, child.Name(), spanNames(sr))
	return nil
}

func spanNames(sr *tracetest.SpanRecorder) string {
	out := ""
	for _, s := range sr.Ended() {
		if out != "" {
			out += ", "
		}
		out += s.Name()
	}
	return "[" + out + "]"
}

// spanAttr returns the string value of key on span, or "" with found=false.
func spanAttr(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestRequestPathSpans_HierarchyAndVocabulary covers ACs 1, 4 and 5 in one pass
// over one real call: four or more named spans, forming a single connected
// trace in the documented parent order, with correlation_id and outcome on the
// dispatch span.
func TestRequestPathSpans_HierarchyAndVocabulary(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	session := tracedSession(t, &compositeStubHandler{}, broker.URL, true)

	callProbeTool(t, session)

	dispatch := oneSpan(t, sr, "tools.CallTool")
	executor := oneSpan(t, sr, "composite.Execute")
	sempSpan := oneSpan(t, sr, "semp.request")
	entry := entrySpanParenting(t, sr, dispatch)

	// "4 or more NAMED spans" — a bare count would pass against four
	// anonymous spans nobody can query by.
	if got := len(sr.Ended()); got < 4 {
		t.Fatalf("ended spans = %d, want >= 4; got %s", got, spanNames(sr))
	}

	// Edge by edge: a "same trace ID" check alone passes on a flat trace of
	// four sibling roots, exactly the shape a stray context.Background() makes.
	for _, edge := range []struct {
		child, parent sdktrace.ReadOnlySpan
		childName     string
		parentName    string
	}{
		{executor, dispatch, "composite.Execute", "tools.CallTool"},
		{sempSpan, executor, "semp.request", "composite.Execute"},
	} {
		if edge.child.Parent().SpanID() != edge.parent.SpanContext().SpanID() {
			t.Errorf("%s parent = %v, want %s (%v)", edge.childName,
				edge.child.Parent().SpanID(), edge.parentName,
				edge.parent.SpanContext().SpanID())
		}
	}

	// One trace ID across all four, or an operator finds fragments.
	for _, s := range []sdktrace.ReadOnlySpan{dispatch, executor, sempSpan} {
		if s.SpanContext().TraceID() != entry.SpanContext().TraceID() {
			t.Errorf("%s trace ID = %s, want the entry span's %s", s.Name(),
				s.SpanContext().TraceID(), entry.SpanContext().TraceID())
		}
	}

	if entry.SpanKind() != trace.SpanKindServer {
		t.Errorf("entry span kind = %v, want Server (OTel HTTP server convention)", entry.SpanKind())
	}
	if sempSpan.SpanKind() != trace.SpanKindClient {
		t.Errorf("semp.request kind = %v, want Client (outbound call)", sempSpan.SpanKind())
	}

	// The attributes that make a span identifiable, each previously asserted
	// nowhere. semp.operation is what distinguishes one SEMP span from the next
	// in a trace, and it carries a security argument — the spec operationId,
	// never the resolved URL, whose path parameters interpolate customer
	// topology (VPN and queue names). That argument had no regression guard, so
	// a well-meaning change to "make the span more useful" by recording the URL
	// would not have failed anything.
	for _, want := range []struct {
		span       sdktrace.ReadOnlySpan
		key, value string
	}{
		{executor, "tool", "span-probe-tool"},
		{sempSpan, "semp.version", "v2"},
		{sempSpan, "semp.operation", "getMsgVpn"},
		{sempSpan, "http.request.method", http.MethodGet},
	} {
		if got, _ := spanAttr(want.span, want.key); got != want.value {
			t.Errorf("%s %s = %q, want %q", want.span.Name(), want.key, got, want.value)
		}
	}
	if steps, ok := intSpanAttr(executor, "composite.steps"); !ok || steps != 1 {
		t.Errorf("composite.Execute composite.steps = %d (present: %v), want 1", steps, ok)
	}
	// No attribute may carry the resolved URL or the broker's address: path
	// params interpolate customer topology, and a span exports offsite.
	for _, s := range []sdktrace.ReadOnlySpan{dispatch, executor, sempSpan} {
		for _, kv := range s.Attributes() {
			if v := kv.Value.String(); strings.Contains(v, broker.URL) || strings.Contains(v, "/SEMP/") {
				t.Errorf("%s attribute %s = %q exports the resolved SEMP URL; %s must carry the operationId only",
					s.Name(), kv.Key, v, "semp.operation")
			}
		}
	}

	// The join key to this call's audit record and log lines.
	corrID, ok := spanAttr(dispatch, "correlation_id")
	if !ok || corrID == "" {
		t.Errorf("tools.CallTool has no correlation_id attribute; attrs: %v", dispatch.Attributes())
	}
	if entryID, _ := spanAttr(entry, "correlation_id"); entryID != corrID {
		t.Errorf("entry span correlation_id = %q, dispatch = %q; one call must carry one ID", entryID, corrID)
	}

	if got, _ := spanAttr(dispatch, "outcome"); got != "success" {
		t.Errorf("tools.CallTool outcome = %q, want %q", got, "success")
	}
	// Documented as present ONLY on outcome=error: an empty error_type on a
	// success would match a predicate on the empty string.
	if _, ok := spanAttr(dispatch, "error_type"); ok {
		t.Errorf("tools.CallTool set error_type on a successful call; it is documented as present on outcome=error only")
	}
}

// documentedErrorTypes is the closed error_type vocabulary a span may report,
// derived from metrics.AllErrorTypes rather than re-listed here. An earlier
// version of this file hand-wrote the set beside a comment claiming it was
// derived; it was not, and a const added to the type while this copy lagged
// produced `other` on the metric and the real value on the span for the same
// call, with nothing failing in CI. Deriving it removes that class of drift
// instead of documenting it.
//
// ErrorTypeOther is dropped on purpose: it is the sentinel Record coerces an
// off-vocabulary value to, so a span reporting it would mean the classifier
// produced something the closed set does not cover — a failure, not a valid
// value.
var documentedErrorTypes = func() map[metrics.ErrorType]bool {
	m := map[metrics.ErrorType]bool{}
	for _, et := range metrics.AllErrorTypes() {
		if et == metrics.ErrorTypeOther {
			continue
		}
		m[et] = true
	}
	return m
}()

// TestRequestPathSpans_ErrorVocabulary pins the other half: a failed call
// reports outcome=error with the cause in error_type, from the closed set of
// metrics.ErrorType values. An SRE carries that value from a dashboard into the
// trace backend unchanged, so it must be one of the closed set, not a Go error.
func TestRequestPathSpans_ErrorVocabulary(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	session := tracedSession(t, &compositeStubHandler{failStep: true}, broker.URL, true)

	// The tool fails, but returns a structured error result rather than a
	// protocol error, so CallTool itself does not error.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "span-probe-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	}); err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}

	dispatch := oneSpan(t, sr, "tools.CallTool")
	if got, _ := spanAttr(dispatch, "outcome"); got != "error" {
		t.Fatalf("outcome = %q, want %q", got, "error")
	}
	gotType, ok := spanAttr(dispatch, "error_type")
	if !ok {
		t.Fatalf("outcome=error span has no error_type; attrs: %v", dispatch.Attributes())
	}
	// Membership only: this asserts the one value THIS path produced is in the
	// vocabulary, not that the vocabulary itself is complete. Completeness is
	// enforced where the set is declared —
	// metrics.TestAllErrorTypes_CoversEveryDeclaredConst parses the const block
	// so a new const cannot be added without updating the set, and
	// TestErrorTypeVocabulary_MatchesDocumentedTable below holds the docs to
	// the same source.
	if !documentedErrorTypes[metrics.ErrorType(gotType)] {
		t.Errorf("error_type = %q, which is not one of the documented metrics.ErrorType values", gotType)
	}
	if got := dispatch.Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error", got)
	}
	// No raw error text: it can quote a broker response, and spans export
	// offsite. error_type carries the classification.
	if desc := dispatch.Status().Description; desc != "" {
		t.Errorf("span status description = %q, want empty (unvouched error text must not be exported)", desc)
	}
}

// This test and its sibling below are AC 3, which asks for both cases. They are
// why the propagator install in tracing.New exists: before it, an inbound
// traceparent was silently ignored and every entry span was a root, with no
// error and no log line to distinguish that from working propagation.
func TestRequestPathSpans_InboundTraceparentContinuesUpstreamTrace(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	pool := spanPoolFor(t, broker.URL)
	mgr := tools.NewToolManager(pool)
	mgr.Register(&compositeStubHandler{})
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")

	var handler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil)
	handler = correlation.Middleware(tracing.HTTPMiddleware("/mcp", handler))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// A traceparent an upstream agent would send: version 00, sampled.
	const upstreamTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	const upstreamSpan = "00f067aa0ba902b7"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("traceparent", "00-"+upstreamTrace+"-"+upstreamSpan+"-01")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	_ = resp.Body.Close()

	entry := oneSpan(t, sr, "POST /mcp")
	if got := entry.SpanContext().TraceID().String(); got != upstreamTrace {
		t.Errorf("entry span trace ID = %s, want the upstream trace %s — the inbound traceparent was not extracted",
			got, upstreamTrace)
	}
	if got := entry.Parent().SpanID().String(); got != upstreamSpan {
		t.Errorf("entry span parent = %s, want the upstream span %s", got, upstreamSpan)
	}
	if !entry.Parent().IsRemote() {
		t.Error("entry span parent is not marked remote; it came from an inbound header, not this process")
	}
}

// TestRequestPathSpans_NoTraceparentStartsNewRoot is the "when absent" half.
func TestRequestPathSpans_NoTraceparentStartsNewRoot(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	session := tracedSession(t, &compositeStubHandler{}, broker.URL, true)

	callProbeTool(t, session)

	entry := entrySpanParenting(t, sr, oneSpan(t, sr, "tools.CallTool"))
	if entry.Parent().SpanID().IsValid() {
		t.Errorf("entry span has parent %v, want a new root (no inbound traceparent was sent)",
			entry.Parent().SpanID())
	}
	if !entry.SpanContext().TraceID().IsValid() {
		t.Error("entry span has no valid trace ID")
	}
}

// TestRequestPathSpans_NoEntrySpanWhenTracingDisabled covers the chain half of
// AC 6: with tracingEnabled false, buildMCPEndpoint omits the middleware, so
// there is no entry span and the dispatch span has no server-side parent.
//
// Scope note: this cannot assert "no spans at all", because this binary
// installs an always-sampling provider for the tests above and the inner layers
// call tracer.Start unconditionally. In production the flag-off path installs
// no provider, leaving OTel's non-recording default. That half is pinned where
// the flag lives — TestNew_Disabled_* in internal/observability/tracing.
func TestRequestPathSpans_NoEntrySpanWhenTracingDisabled(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	session := tracedSession(t, &compositeStubHandler{}, broker.URL, false)

	callProbeTool(t, session)

	for _, s := range sr.Ended() {
		if s.Name() == "POST /mcp" {
			t.Errorf("an entry span was created with the tracing middleware omitted from the chain")
		}
	}
	// With no entry span, the dispatch span is a root.
	if dispatch := oneSpan(t, sr, "tools.CallTool"); dispatch.Parent().SpanID().IsValid() {
		t.Errorf("tools.CallTool has parent %v with tracing off, want none",
			dispatch.Parent().SpanID())
	}
}

// TestRequestPathSpans_SSEStreamIsNotSpanned pins the fix for a real defect
// found reviewing this story. The MCP streamable transport opens a GET /mcp SSE
// channel that stays open for the whole session, so tracing it held a span open
// for the session's lifetime: unexported until close, lost if the process died,
// and a duration that swamps any latency view built on entry-span duration.
// HTTPMiddleware filters GET for that reason.
//
// Asserted as "no span is ever STARTED for the stream", deliberately not as
// "every started span has ended".
//
// The latter is how the defect was originally found, but it does not survive as
// an assertion: it samples started-minus-ended at one instant with the session
// still open, and the transport's own notification POSTs can legitimately be in
// flight right then. That makes it a timing flake whose message would read "a
// long-lived stream is being spanned" — the most misleading possible diagnostic
// for a race. Closing the session first would remove the race and the value
// together, since a leaked stream span ends at close and the check would pass
// with the bug present.
//
// The started-span check has neither problem: if the filter stops matching, a
// span for the stream is created immediately and deterministically.
func TestRequestPathSpans_SSEStreamIsNotSpanned(t *testing.T) {
	sr := recordRequestPathSpans(t)
	broker := fakeBroker(t)
	session := tracedSession(t, &compositeStubHandler{}, broker.URL, true)

	callProbeTool(t, session)

	for _, st := range sr.Started() {
		if st.Name() == "GET /mcp" {
			t.Error("the SSE notification stream was spanned: it stays open for the whole session, so its span would be reported only at session end, lost entirely if the process died first, and long enough to swamp any latency view built on entry-span duration")
		}
	}
	// Guard against the filter over-matching into a no-op middleware: the
	// request POSTs must still be traced.
	if len(sr.Ended()) == 0 {
		t.Fatal("no spans at all; the filter is rejecting everything")
	}
	if _, ok := findSpan(sr, "POST /mcp"); !ok {
		t.Error("no POST /mcp entry span: the filter is excluding requests, not just the stream")
	}
}

// findSpan returns the last ended span with the given name.
func findSpan(sr *tracetest.SpanRecorder, name string) (sdktrace.ReadOnlySpan, bool) {
	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			found = s
		}
	}
	return found, found != nil
}

// toolMetricSeries scrapes p and returns the label sets of every
// mcp_tool_invocation_total series, keyed nothing-in-particular — the caller
// asserts against the one series its single call produced.
func toolMetricSeries(t *testing.T, p *metrics.Provider) []map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))

	var out []map[string]string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		rest, ok := strings.CutPrefix(line, "mcp_tool_invocation_total{")
		if !ok {
			continue
		}
		labels, _, ok := strings.Cut(rest, "}")
		if !ok {
			t.Fatalf("malformed series line: %q", line)
		}
		kv := map[string]string{}
		for _, pair := range strings.Split(labels, ",") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				t.Fatalf("malformed label %q in %q", pair, line)
			}
			kv[k] = strings.Trim(v, `"`)
		}
		out = append(out, kv)
	}
	return out
}

// TestRequestPathSpans_SpanMetricAndLogAgreeOnTheSameCall is SOL-152421's
// cross-signal criterion, widened by SOL-154036 to the third surface.
//
// The promise: an operator copies the label values off a spiking
// `mcp_tool_invocation_total` series into their trace backend and their SIEM
// and lands on the same call. Nothing in the type system enforces that — an
// `attribute.String`, a Prometheus label and an `slog.String` from three call
// sites — so the surfaces can drift while each still looks healthy alone.
//
// The third surface here is logToolResult's `tool invoked` line, emitted for
// every dispatch. The narrower `audit_event_type=operation` record is joined
// by TestRequestPathSpans_OperationAuditRecordAgreesWithSpanAndMetric.
//
// One real call per outcome, so all three sides come from the same dispatch
// rather than from fixtures that agree by construction.
func TestRequestPathSpans_SpanMetricAndLogAgreeOnTheSameCall(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failStep bool
		panics   bool
		broker   string
		outcome  string
	}{
		{name: "success", failStep: false, broker: "dev", outcome: "success"},
		{name: "error", failStep: true, broker: "dev", outcome: "error"},
		// The resolution-failure path, which is where the two surfaces are
		// easiest to get wrong: the call fails before the alias is normalised
		// to its configured form, so the raw value here is whatever string the
		// caller typed. The metric has always bounded it to the `unknown`
		// sentinel (an unbounded label would be a cardinality bomb); the span
		// has to bound it identically or the join breaks precisely on the
		// requests an operator is chasing. It also keeps arbitrary caller
		// input from egressing to the collector as a span attribute.
		{name: "unknown broker", broker: "no-such-broker-" + strings.Repeat("x", 8), outcome: "error"},
		// The panic path, which reaches its classification differently from
		// every other case: CallTool's audit defer infers it after the fact
		// (both toolErr and result still nil means the handler panicked) and
		// rewrites errorType to `panic`. The span defer is registered BEFORE
		// that one so it runs AFTER it, and therefore reports the rewritten
		// value. Registered the other way round the span would say nothing
		// while the metric and the audit record both said `panic` — a silent
		// disagreement on the one outcome an operator is most likely to chase.
		{name: "panic", panics: true, broker: "dev", outcome: "error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, labels, logged := crossSignalCall(t,
				&compositeStubHandler{failStep: tt.failStep, panics: tt.panics}, tt.broker)

			if labels["outcome"] != tt.outcome {
				t.Fatalf("metric outcome = %q, want %q", labels["outcome"], tt.outcome)
			}

			// The four keys both surfaces carry. correlation_id is span-only
			// (per-request, so unbounded as a metric label) and is excluded on
			// purpose rather than overlooked.
			for _, key := range []string{"tool", "broker", "outcome", "error_type"} {
				// An absent span attribute compares as "": Prometheus cannot
				// express an absent label, so "" is its encoding of one, and
				// error_type is empty on a successful call.
				spanValue, _ := spanAttr(dispatch, key)
				if spanValue != labels[key] {
					t.Errorf("%s: span = %q, metric label = %q — the two surfaces disagree about the same call, so an operator's metric-to-trace filter finds nothing",
						key, spanValue, labels[key])
				}
			}

			// Guards the loop above against passing vacuously: if both sides
			// were empty for every key it would report no failure at all.
			if labels["tool"] == "" || labels["broker"] == "" {
				t.Errorf("metric labels tool=%q broker=%q: one is empty, so the comparison above proves little",
					labels["tool"], labels["broker"])
			}
			if tt.outcome == "error" && labels["error_type"] == "" {
				t.Error("metric error_type is empty on an error outcome; nothing was compared for it")
			}

			// `broker` is deliberately excluded: logToolResult logs the RAW
			// caller alias for diagnostics while the metric and the span are
			// canonicalized, so on the unknown-broker case they differ by
			// design. Asserting equality would pin that divergence shut.
			line := oneRecordWithMsg(t, logged, "tool invoked")
			for _, key := range []string{"tool", "outcome", "error_type"} {
				if got := stringField(line, key); got != labels[key] {
					t.Errorf("%s: log line = %q, metric label = %q — a SIEM query carried over from a dashboard finds nothing",
						key, got, labels[key])
				}
			}
		})
	}
}

// TestRequestPathSpans_BypassDispatchSitesSpan covers the dispatch paths that
// never reach ToolManager.CallTool, and so do not get their span from it:
// `list-brokers`, which is registered straight against the MCP server, and the
// argument-parse failure in register.go's instrumented closure, which returns
// before dispatching.
//
// Both already emit their own audit line and their own metric, each with a
// comment saying they must because they bypass CallTool. The span is the third
// signal and needs the same treatment: without it `list-brokers` appears in
// every dashboard and in no trace, and `bad_request` is a metric-only value of
// a vocabulary docs/observability.md documents as shared by all three. The
// operator-visible failure is silent — the filter carried over from the metric
// simply matches nothing.
func TestRequestPathSpans_BypassDispatchSitesSpan(t *testing.T) {
	for _, tt := range []struct {
		name          string
		call          *mcp.CallToolParams
		wantTool      string
		wantOutcome   string
		wantErrorType string
	}{
		{
			name:        "list-brokers is registered outside the manager",
			call:        &mcp.CallToolParams{Name: "list-brokers"},
			wantTool:    "list-brokers",
			wantOutcome: "success",
		},
		{
			// Arguments that are valid JSON but not an object: the closure's
			// json.Unmarshal into map[string]any fails and it returns without
			// ever calling CallTool.
			name:          "argument-parse failure returns before dispatch",
			call:          &mcp.CallToolParams{Name: "span-probe-tool", Arguments: []any{"not", "an", "object"}},
			wantTool:      "span-probe-tool",
			wantOutcome:   "error",
			wantErrorType: "bad_request",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sr := recordRequestPathSpans(t)
			p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
			tm, err := p.ToolMetrics()
			if err != nil {
				t.Fatal(err)
			}

			broker := fakeBroker(t)
			session := tracedSessionWith(t, &compositeStubHandler{}, broker.URL, true, tm)

			if _, err := session.CallTool(context.Background(), tt.call); err != nil {
				t.Fatalf("CallTool returned a protocol error: %v", err)
			}

			dispatch := oneSpan(t, sr, "tools.CallTool")
			if got, _ := spanAttr(dispatch, "tool"); got != tt.wantTool {
				t.Errorf("span tool = %q, want %q", got, tt.wantTool)
			}
			if got, _ := spanAttr(dispatch, "outcome"); got != tt.wantOutcome {
				t.Errorf("span outcome = %q, want %q", got, tt.wantOutcome)
			}
			if got, _ := spanAttr(dispatch, "error_type"); got != tt.wantErrorType {
				t.Errorf("span error_type = %q, want %q", got, tt.wantErrorType)
			}

			// The point of the span: the values an operator carries over from
			// the metric must match it, exactly as they do for a call that
			// does go through CallTool.
			series := toolMetricSeries(t, p)
			if len(series) != 1 {
				t.Fatalf("mcp_tool_invocation_total series = %d, want 1: %v", len(series), series)
			}
			for _, key := range []string{"tool", "outcome", "error_type", "broker"} {
				spanValue, _ := spanAttr(dispatch, key)
				if spanValue != series[0][key] {
					t.Errorf("%s: span = %q, metric label = %q", key, spanValue, series[0][key])
				}
			}
		})
	}
}

// TestErrorTypeVocabulary_MatchesDocumentedTable holds docs/observability.md to
// the same source as the code.
//
// The error_type table is what an operator actually reads before writing a
// dashboard filter, and it is the copy of this vocabulary furthest from the
// consts — so it is the one that drifts. It already had: the table said twelve
// values while two other paragraphs in the same file still said ten, and a row
// for `broker_permission_denied` survived a merge that did not add the const,
// documenting a value Record would have coerced to `other`.
//
// Parsing the table rather than duplicating it means the doc and the code move
// together, which is what SOL-152421's own note assumes when it says this
// vocabulary's test "will fail when Story 49 lands — by design".
func TestErrorTypeVocabulary_MatchesDocumentedTable(t *testing.T) {
	const docPath = "../../docs/observability.md"
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}

	// The table under the error_type heading: rows are `| `value` | meaning |`.
	// Anchored on the sentence that introduces it so another table's rows
	// cannot be picked up instead.
	// Anchored on the section heading, not on the prose that introduces the
	// table: "drawn from a closed set of" also appears above the authz_denied
	// `reason` table, and anchoring there swept in every table between the two.
	body := string(doc)
	anchor := strings.Index(body, "### `error_type`")
	if anchor == -1 {
		t.Fatal("could not find the `### `error_type`` heading in docs/observability.md; if it was renamed, update this anchor")
	}
	rest := body[anchor:]
	end := strings.Index(rest, "\nNotes:")
	if end == -1 {
		t.Fatal("could not find the end of the error_type table (expected a following \"Notes:\" block)")
	}

	// Anchored on the full row shape — a backticked value in the FIRST cell,
	// followed by the cell separator — so the prose that follows the table
	// (which cites several of these values inline) cannot be read as rows. A
	// whitespace-delimited scan did exactly that, and reported a value of
	// "execution_error`," from a sentence.
	rowValue := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	documented := map[string]bool{}
	rowCount := 0
	for _, m := range rowValue.FindAllStringSubmatch(rest[:end], -1) {
		rowCount++
		documented[m[1]] = true
	}
	// Row count, not just the set: merging two branches that each reworded a
	// row left this table with fourteen rows and twelve distinct values, which
	// a set comparison alone reads as correct.
	if rowCount != len(documented) {
		t.Errorf("error_type table has %d rows but %d distinct values: a value is documented twice", rowCount, len(documented))
	}
	if len(documented) == 0 {
		t.Fatal("parsed no rows from the error_type table; this test cannot protect anything")
	}

	want := map[string]bool{}
	for et := range documentedErrorTypes {
		want[string(et)] = true
	}

	for value := range want {
		if !documented[value] {
			t.Errorf("error_type %q is in the code vocabulary but has no row in the docs/observability.md table: an operator reading the docs would not know it can appear", value)
		}
	}
	for value := range documented {
		if !want[value] {
			t.Errorf("the docs/observability.md error_type table documents %q, which is not in metrics.AllErrorTypes: nothing can emit it, and Record would coerce it to %q",
				value, metrics.ErrorTypeOther)
		}
	}

	// The prose count has to agree with the rows, since the two drifted before.
	countWord := map[int]string{10: "ten", 11: "eleven", 12: "twelve", 13: "thirteen", 14: "fourteen"}[len(want)]
	if countWord == "" {
		t.Fatalf("no spelled-out word known for %d values; extend countWord", len(want))
	}
	if wrong := regexp.MustCompile(`closed set of (\w+) values`).FindAllStringSubmatch(body, -1); wrong != nil {
		for _, m := range wrong {
			if m[1] != countWord {
				t.Errorf("docs/observability.md says %q but the vocabulary has %d values (%q)", m[0], len(want), countWord)
			}
		}
	}
	// Several phrasings, because each of these has drifted at least once:
	// "twelve-value `error_type` set", "`error_type` of twelve values", "the
	// twelve `error_type` values", and "ten-value `error_type` vocabulary" —
	// the last two were still saying "ten" while the table said twelve.
	for _, pattern := range []string{
		`(\w+)-value \x60error_type\x60 (?:set|vocabulary)`,
		`\x60error_type\x60 of (\w+) values`,
		`the (\w+)\s+\x60error_type\x60 values`,
	} {
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(body, -1) {
			if m[1] != countWord {
				t.Errorf("docs/observability.md says %q but the vocabulary has %d values (%q)", strings.Join(strings.Fields(m[0]), " "), len(want), countWord)
			}
		}
	}
}

// intSpanAttr reads an int-valued span attribute.
func intSpanAttr(s sdktrace.ReadOnlySpan, key string) (int64, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInt64(), true
		}
	}
	return 0, false
}
