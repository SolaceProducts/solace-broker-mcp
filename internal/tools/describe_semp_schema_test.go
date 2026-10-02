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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// Smoke tests against the embedded specs: trimmed POST, trimmed PATCH,
// raw view, unknown operation.

func TestSempSchemaMap_BuildsFromEmbeddedSpecs(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	// All ops tools.yaml points at from create/update descriptions; a spec
	// upgrade that drops one turns the pointer into a dead link.
	for _, opKey := range []string{
		"config/createMsgVpn",
		"config/updateMsgVpn",
		"config/createMsgVpnQueue",
		"config/updateMsgVpnQueue",
		"config/createMsgVpnTopicEndpoint",
		"config/updateMsgVpnTopicEndpoint",
		"config/createMsgVpnRestDeliveryPoint",
		"config/updateMsgVpnRestDeliveryPoint",
	} {
		info, ok := reg.ops[opKey]
		if !ok {
			t.Errorf("operation %q missing from semp schema map", opKey)
			continue
		}
		if info.defName == "" {
			t.Errorf("operation %q has no body definition", opKey)
		}
	}
}

// The tool describes writable request bodies; the monitor API is all GETs with
// no bodies. Loading it would just add empty entries and mislead agents into
// thinking monitor operations are queryable here.
func TestSempSchemaMap_ExcludesMonitorSpec(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	if _, ok := reg.specs["monitor"]; ok {
		t.Errorf("monitor spec should not be loaded")
	}
	for key := range reg.ops {
		if strings.HasPrefix(key, "monitor/") {
			t.Errorf("monitor operation indexed: %q", key)
		}
	}
}

func TestSempSchemaMap_TrimmedView_CreateQueue(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/createMsgVpnQueue", "trimmed")
	if err != nil {
		t.Fatalf("describe(trimmed): %v", err)
	}
	if got["method"] != "POST" || got["definition"] != "MsgVpnQueue" {
		t.Errorf("unexpected header fields: %+v", got)
	}
	attrs := got["attributes"].([]map[string]any)
	byName := make(map[string]map[string]any, len(attrs))
	for _, a := range attrs {
		byName[a["name"].(string)] = a
	}

	// queueName is the identifying path param: required on create, read-only on update.
	qn := byName["queueName"]
	if qn["identifying"] != true || qn["requiredForCreate"] != true {
		t.Errorf("queueName should be identifying + requiredForCreate: %+v", qn)
	}
	if qn["writableOnUpdate"] != false {
		t.Errorf("queueName should be read-only on update: %+v", qn)
	}

	// msgVpnName is read-only on create (injected from the path, not the request body).
	mvn := byName["msgVpnName"]
	if mvn["writableOnCreate"] != false {
		t.Errorf("msgVpnName should have writableOnCreate=false: %+v", mvn)
	}

	// permission carries the enum + default the trimmed view is meant to surface.
	perm := byName["permission"]
	if perm["default"] != "no-access" {
		t.Errorf("permission default: got %v want no-access", perm["default"])
	}
	enum, ok := perm["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Errorf("permission enum missing or empty: %+v", perm["enum"])
	}
	// Trimmed description drops the "minimum access scope" boilerplate but keeps
	// the <pre> enum block so callers know what each enum value means.
	desc, _ := perm["description"].(string)
	if strings.Contains(desc, "minimum access scope") {
		t.Errorf("trimmed description should not contain access-scope boilerplate; got: %q", desc)
	}
	if !strings.Contains(desc, "<pre>") {
		t.Errorf("trimmed description should include <pre> enum block; got: %q", desc)
	}

	// permission.x-autoDisable is non-empty: changing permission on a live queue
	// briefly sets egressEnabled=false. An agent must see this before invoking update.
	if perm["autoDisable"] == nil {
		t.Errorf("permission should have autoDisable field: %+v", perm)
	}

	// eventBindCountThreshold is a bare $ref — must resolve to nested properties,
	// not fabricate writability flags from absent extensions.
	thresh := byName["eventBindCountThreshold"]
	if thresh == nil {
		t.Fatalf("eventBindCountThreshold missing from trimmed output")
	}
	if thresh["type"] != "object" {
		t.Errorf("$ref property should have type=object: %+v", thresh)
	}
	if _, hasWritable := thresh["writableOnCreate"]; hasWritable {
		t.Errorf("$ref property must not carry fabricated writableOnCreate: %+v", thresh)
	}
	nestedProps, ok := thresh["properties"].([]map[string]any)
	if !ok || len(nestedProps) == 0 {
		t.Errorf("$ref property should have resolved nested properties: %+v", thresh)
	}
}

func TestDescribeSempSchema_EmitsAuditLog(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	if err := RegisterDescribeSempSchema(mgr, specs.FS); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "describe-semp-schema",
		Arguments: map[string]any{"operation": "config/createMsgVpnQueue"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("describe-semp-schema returned IsError: %v", res.Content)
	}

	audits := auditLines(t, &logBuf, "describe-semp-schema")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for describe-semp-schema, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "success" {
		t.Errorf("audit outcome = %v, want %q", got, "success")
	}
	// describe-semp-schema resolves no broker; the audit line carries the "none"
	// sentinel, matching the metric label rather than omitting the field.
	if got := audits[0]["broker"]; got != "none" {
		t.Errorf("audit broker = %v, want %q", got, "none")
	}
}

// newDescribeSempSchemaSession spins up a real MCP server+client session for
// describe-semp-schema over an in-memory transport (same shape as
// TestDescribeSempSchema_EmitsAuditLog), wired to a fresh *metrics.Provider so
// a test can scrape the mcp_tool_invocation_total series it records in
// isolation — the underlying counter is cumulative for the provider's
// lifetime, so each test case needs its own provider to assert a bare "== 1"
// occurrence rather than accounting for accumulation across cases.
// clientMiddleware, if given, is installed on the client via
// AddSendingMiddleware before Connect (mirrors callToolTestHarness in
// register_test.go).
func newDescribeSempSchemaSession(t *testing.T, logBuf *bytes.Buffer, clientMiddleware ...mcp.Middleware) (*mcp.ClientSession, *metrics.Provider) {
	t.Helper()
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	// Metrics are wired onto mgr, not passed to RegisterDescribeSempSchema
	// directly (SOL-153693): once the tool is routed through ToolManager,
	// CallTool's own defer is what records every invocation, the same single
	// site every other tool uses.
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool, WithToolMetrics(tm))
	if err := RegisterDescribeSempSchema(mgr, specs.FS); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	if len(clientMiddleware) > 0 {
		client.AddSendingMiddleware(clientMiddleware...)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, p
}

// forceMalformedArguments is client-sending middleware that overwrites
// CallToolParams.Arguments with a syntactically valid but semantically
// wrong-typed JSON literal (a bare string) immediately before the request is
// marshaled and sent. Unlike forceOmitArguments (register_test.go), which
// reproduces a request with no "arguments" field at all, this reproduces a
// request whose "arguments" field is present but is not a JSON object.
//
// Before SOL-153693 this was the only way to make
// json.Unmarshal(req.Params.Arguments, &args) fail inside
// describe_semp_schema.go's own handler, since a normal client call always
// sends a well-formed JSON object. Now that the tool is routed through
// ToolManager, that unmarshal happens once, generically, in
// RegisterWithServer's dispatch closure (register.go) — the same guard every
// tool shares — so this middleware exercises the shared path, not a
// describe-semp-schema special case.
func forceMalformedArguments(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if p, ok := req.GetParams().(*mcp.CallToolParams); ok {
				p.Arguments = json.RawMessage(`"not an object"`)
			}
		}
		return next(ctx, method, req)
	}
}

// assertDescribeSempSchemaErrorType asserts that describe-semp-schema's audit
// line and the mcp_tool_invocation_total counter both carry
// error_type=wantErrorType for a single invocation — the two independent
// surfaces logToolResult and recordToolInvocation each write to from the same
// deferred call site. Before SOL-153693 that site was a bespoke defer in
// describe_semp_schema.go; now it is ToolManager.CallTool's (manager.go),
// the same one every other tool goes through.
func assertDescribeSempSchemaErrorType(t *testing.T, logBuf *bytes.Buffer, p *metrics.Provider, wantErrorType metrics.ErrorType) {
	t.Helper()

	audits := auditLines(t, logBuf, describeSempSchemaToolName)
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for %s, got %d: %s", describeSempSchemaToolName, len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "error" {
		t.Errorf("audit outcome = %v, want %q", got, "error")
	}
	if got := audits[0]["error_type"]; got != string(wantErrorType) {
		t.Errorf("audit error_type = %v, want %q", got, wantErrorType)
	}

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	want := fmt.Sprintf(`mcp_tool_invocation_total{broker="none",error_type="%s",outcome="error",tool="%s"} 1`,
		wantErrorType, describeSempSchemaToolName)
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Errorf("scrape missing %s series.\nwant line: %s\n--- got ---\n%s", wantErrorType, want, body)
	}
}

// assertDescribeSempSchemaIsError checks that a describe-semp-schema bad-input
// call came back as an isError tool result — never a JSON-RPC protocol error —
// whose message contains every wantSubstrs entry. Before SOL-153693 these four
// cases were protocol errors (code -32602 after SOL-153692, still the wrong
// KIND of error under the MCP spec, which puts a schema-invalid or
// business-logic tool failure in the isError bucket so the model can read it
// and self-correct). Routing the tool through ToolManager closes that gap for
// all four at once.
func assertDescribeSempSchemaIsError(t *testing.T, res *mcp.CallToolResult, err error, wantSubstrs ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error instead of a tool result: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an isError tool result, got success: %#v", res)
	}
	if len(res.Content) == 0 {
		t.Fatal("isError result carries no content; the model has nothing to read")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result.Content[0] = %#v, want *mcp.TextContent", res.Content[0])
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(text.Text, want) {
			t.Errorf("result content = %q, want it to contain %q", text.Text, want)
		}
	}
}

// TestDescribeSempSchema_UnparseableArguments_ErrorTypeReachesAuditAndMetric
// covers the "arguments" field being present on the wire but not a JSON
// object. Before SOL-153693 this was describe_semp_schema.go's own
// json.Unmarshal-failure branch, reported as a protocol error with its own
// "parsing tool arguments" wording. Now the tool is routed through
// ToolManager like every other tool, so this trips the one shared guard every
// tool shares (RegisterWithServer's dispatch closure, register.go) — hence
// the generic message and error_type, not a describe-semp-schema special
// case, which is the point of this ticket.
func TestDescribeSempSchema_UnparseableArguments_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf, forceMalformedArguments)

	ctx := context.Background()
	// forceMalformedArguments overwrites Arguments right before send, so the
	// value supplied here is irrelevant to what actually reaches the wire.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: describeSempSchemaToolName})
	assertDescribeSempSchemaIsError(t, res, err, "tool arguments must be a JSON object")

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeBadRequest)
}

// TestDescribeSempSchema_MissingOperation_ErrorTypeReachesAuditAndMetric
// covers the arguments object parsing fine but carrying no "operation" key.
// ToolManager's input-schema validation catches this before Handle ever runs
// (SOL-153693) — operation is a required property — so error_type is
// validation_error, the same value every other tool's missing-required-
// parameter case gets, and the message is gojsonschema's own wording behind
// this package's "parameter validation failed" prefix, not describe-semp-
// schema's retired hand-written text.
func TestDescribeSempSchema_MissingOperation_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      describeSempSchemaToolName,
		Arguments: map[string]any{},
	})
	assertDescribeSempSchemaIsError(t, res, err, "parameter validation failed", "operation")

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeValidationError)
}

// TestDescribeSempSchema_InvalidView_ErrorTypeReachesAuditAndMetric covers
// operation present but view neither "trimmed" nor "raw". ToolManager's input
// schema declares view as an enum of exactly those two values, so this is
// also caught by validation before Handle runs (SOL-153693), landing on the
// same validation_error classification as the missing-operation case above.
func TestDescribeSempSchema_InvalidView_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: describeSempSchemaToolName,
		Arguments: map[string]any{
			"operation": "config/createMsgVpnQueue",
			"view":      "bogus",
		},
	})
	assertDescribeSempSchemaIsError(t, res, err, "parameter validation failed", "view")

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeValidationError)
}

// TestDescribeSempSchema_UnknownOperation_ErrorTypeReachesAuditAndMetric
// covers a well-formed operation identifier that isn't in the embedded spec
// index (reg.describe's error return), mirroring
// TestSempSchemaMap_UnknownOperation's input but through the full dispatch
// path rather than a direct call to describe().
//
// Unlike the three cases above, this is a genuine tool-execution error, not
// an input-schema violation — "config/thisDoesNotExist" IS a valid string per
// the schema — so it reaches Handle and flows through ToolManager's generic
// handler-error path. That makes it the one case here that keeps its exact
// pre-SOL-153693 message text: describeOperationError (errors.go) exists
// specifically so buildErrorMessage shows this package's own "unknown
// operation" wording verbatim instead of replacing it with the generic
// broker-error message. error_type is execution_error, the same generic
// classification every other tool's handler error gets — the previous
// not_found value existed only because this tool bypassed ToolManager, and
// retired with it (internal/observability/audit/event.go).
func TestDescribeSempSchema_UnknownOperation_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: describeSempSchemaToolName,
		Arguments: map[string]any{
			"operation": "config/thisDoesNotExist",
		},
	})
	assertDescribeSempSchemaIsError(t, res, err, "unknown operation")

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeExecutionError)
}

func TestSempSchemaMap_TrimmedView_UpdateReflectsMethod(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/updateMsgVpnQueue", "trimmed")
	if err != nil {
		t.Fatalf("describe(trimmed update): %v", err)
	}
	if got["method"] != "PATCH" {
		t.Errorf("method for updateMsgVpnQueue: got %v want PATCH", got["method"])
	}
}

func TestSempSchemaMap_RawView(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/createMsgVpnQueue", "raw")
	if err != nil {
		t.Fatalf("describe(raw): %v", err)
	}
	schema, ok := got["schema"].(map[string]any)
	if !ok {
		t.Fatalf("raw view missing schema object: %+v", got)
	}
	// Raw view must preserve properties AND the x-* extensions that
	// power the trimmed view — that is the round-trip guarantee.
	props := schema["properties"].(map[string]any)
	perm := props["permission"].(map[string]any)
	if _, hasExt := perm["x-default"]; !hasExt {
		t.Errorf("raw view should carry x-default extension; got: %+v", perm)
	}
}

func TestSempSchemaMap_UnknownOperation(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	_, err = reg.describe("config/thisDoesNotExist", "trimmed")
	if err == nil {
		t.Fatal("expected error for unknown operation, got nil")
	}
	if !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("error should identify unknown-operation cause; got: %v", err)
	}
}

// TestDescribeSempSchema_OutputMatchesDeclaredSchema is the gate behind the
// declared output schema (SOL-153694). Since SOL-153693, ToolManager validates
// every real call's structuredContent against this schema too — but only for
// whatever operation and view a given call happens to use. This test exists
// for the breadth that runtime validation alone wouldn't give: every indexed
// operation, in both views, so the declaration cannot drift from what
// describe() actually returns for any of them without being caught here
// first, rather than only for the ones a wire-level test happens to exercise.
//
// Every indexed operation is validated in both views, so the coverage tracks
// the embedded specs rather than a hand-picked sample: an operation whose
// definition uses a shape the schema does not allow fails here.
func TestDescribeSempSchema_OutputMatchesDeclaredSchema(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	// Compiled once and reused: validateAgainstSchema re-parses the schema on
	// every call, which over several thousand documents dominates the test.
	compiled, err := compileSchema(describeSempSchemaOutputSchema())
	if err != nil {
		t.Fatalf("compiling the declared output schema: %v", err)
	}

	ops := make([]string, 0, len(reg.ops))
	for op := range reg.ops {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	if len(ops) == 0 {
		t.Fatal("no operations indexed; the loop below would pass vacuously")
	}

	validated := 0
	for _, op := range ops {
		for _, view := range []string{"trimmed", "raw"} {
			doc, dErr := reg.describe(op, view)
			if dErr != nil {
				t.Errorf("describe(%q, %q): %v", op, view, dErr)
				continue
			}
			if _, vErr := validateAgainstCompiledSchema(doc, compiled, "output validation failed"); vErr != nil {
				t.Errorf("describe(%q, %q) does not validate against the declared outputSchema: %v",
					op, view, vErr)
				continue
			}
			validated++
		}
	}
	t.Logf("validated %d documents across %d operations in 2 views", validated, len(ops))
}

// TestDescribeSempSchema_OutputSchemaRejectsUndeclaredFields proves the schema
// above is actually closed, at every level. Without this, the positive test
// would pass just as happily against a schema that permitted anything, and the
// additionalProperties: false on each level would be decoration.
func TestDescribeSempSchema_OutputSchemaRejectsUndeclaredFields(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	schema := describeSempSchemaOutputSchema()

	base, err := reg.describe("config/createMsgVpnQueue", "trimmed")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if err := ValidateOutput(base, schema); err != nil {
		t.Fatalf("baseline document must validate before mutating it: %v", err)
	}

	t.Run("top level", func(t *testing.T) {
		doc, dErr := reg.describe("config/createMsgVpnQueue", "trimmed")
		if dErr != nil {
			t.Fatalf("describe: %v", dErr)
		}
		doc["unexpectedTopLevelField"] = true
		if err := ValidateOutput(doc, schema); err == nil {
			t.Error("an undeclared top-level field was accepted; " +
				"additionalProperties: false is not in effect at the top level")
		}
	})

	// Depth 1: proves the top-level $ref into #/definitions/attribute resolves.
	// If it did not, an unconstrained item schema would accept this.
	t.Run("attribute level", func(t *testing.T) {
		doc, dErr := reg.describe("config/createMsgVpnQueue", "trimmed")
		if dErr != nil {
			t.Fatalf("describe: %v", dErr)
		}
		attrs, ok := doc["attributes"].([]map[string]any)
		if !ok || len(attrs) == 0 {
			t.Fatalf("attributes is %T with no usable entries; cannot mutate an attribute", doc["attributes"])
		}
		attrs[0]["unexpectedAttributeField"] = true
		if err := ValidateOutput(doc, schema); err == nil {
			t.Error("an undeclared attribute field was accepted; the attributes " +
				"item schema is not being applied")
		}
	})

	// Depth 2. The case above only reaches the attribute definition itself; a
	// nested attribute is reached through that definition's own self-reference,
	// which is a separate resolution. If it silently failed, every $ref-backed
	// attribute's nested properties would go unvalidated and both cases above
	// would still pass.
	//
	// The nesting operation is discovered rather than named: which operations
	// nest is a property of the embedded SEMP specs, not of this tool.
	t.Run("nested attribute level", func(t *testing.T) {
		ops := make([]string, 0, len(reg.ops))
		for op := range reg.ops {
			ops = append(ops, op)
		}
		sort.Strings(ops)

		for _, op := range ops {
			doc, dErr := reg.describe(op, "trimmed")
			if dErr != nil {
				continue
			}
			attrs, ok := doc["attributes"].([]map[string]any)
			if !ok {
				continue
			}
			for _, attr := range attrs {
				nested, nestedOK := attr["properties"].([]map[string]any)
				if !nestedOK || len(nested) == 0 {
					continue
				}
				nested[0]["unexpectedNestedField"] = true
				if err := ValidateOutput(doc, schema); err == nil {
					t.Errorf("%s: an undeclared field on a nested attribute (%v.%v) was "+
						"accepted; the recursive $ref is not being applied at depth 2",
						op, attr["name"], nested[0]["name"])
				}
				return
			}
		}
		t.Fatal("no operation produced a nested attribute list, so the recursive " +
			"$ref is untested; if the specs no longer nest, the recursion in " +
			"describeSempSchemaOutputSchema is dead and should be removed")
	})
}

// TestDescribeSempSchema_RawViewOfBodylessOperationEmitsAttributes pins the
// asymmetry that the declared schema's `attributes` description depends on, and
// that PR #418 review found stated backwards in three places.
//
// describe() returns early for an operation with no request-body definition,
// BEFORE the view is consulted, so that branch emits `attributes` (empty) for
// the raw view as well as the trimmed one. `attributes` is therefore absent
// only from the raw view of an operation that HAS a request body. More than
// half the indexed operations are bodyless, so "absent in the raw view" was
// wrong for the majority of raw calls.
//
// The schema itself was always correct — `attributes` is optional, so an empty
// array validates — which is why the 1,266-document test passed against the
// wrong description. Only a behavioural assertion catches this, hence this test.
//
// It is also a two-way gate. Moving `resp["attributes"]` behind the view check
// would make the original claim true and is arguably the cleaner shape, but it
// changes a response that already ships, so it belongs in its own ticket. If
// someone makes that change, this test fails and points at the three
// descriptions that have to change back with it.
func TestDescribeSempSchema_RawViewOfBodylessOperationEmitsAttributes(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}

	var bodyless, withBody string
	ops := make([]string, 0, len(reg.ops))
	for op := range reg.ops {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	bodylessCount := 0
	for _, op := range ops {
		if reg.ops[op].defName == "" {
			bodylessCount++
			if bodyless == "" {
				bodyless = op
			}
		} else if withBody == "" {
			withBody = op
		}
	}
	if bodyless == "" || withBody == "" {
		t.Fatalf("need one operation of each kind; bodyless=%q withBody=%q", bodyless, withBody)
	}
	t.Logf("%d of %d indexed operations are bodyless; probing %q and %q",
		bodylessCount, len(ops), bodyless, withBody)

	// The bodyless branch ignores the view: both views carry an empty
	// attributes list and a note, and neither carries definition or schema.
	for _, view := range []string{"trimmed", "raw"} {
		doc, dErr := reg.describe(bodyless, view)
		if dErr != nil {
			t.Fatalf("describe(%q, %q): %v", bodyless, view, dErr)
		}
		attrs, ok := doc["attributes"]
		if !ok {
			t.Errorf("describe(%q, %q) omits attributes; the declared schema's "+
				"description says a bodyless operation carries it in BOTH views", bodyless, view)
			continue
		}
		if got, isEmpty := attrs.([]any); !isEmpty || len(got) != 0 {
			t.Errorf("describe(%q, %q) attributes = %#v, want an empty []any",
				bodyless, view, attrs)
		}
		if _, hasNote := doc["note"]; !hasNote {
			t.Errorf("describe(%q, %q) omits note", bodyless, view)
		}
		for _, absent := range []string{"definition", "schema"} {
			if _, has := doc[absent]; has {
				t.Errorf("describe(%q, %q) carries %s; the bodyless branch returns before "+
					"either is set", bodyless, view, absent)
			}
		}
	}

	// With a request body, the raw view is the one case that omits attributes.
	rawDoc, err := reg.describe(withBody, "raw")
	if err != nil {
		t.Fatalf("describe(%q, raw): %v", withBody, err)
	}
	if _, has := rawDoc["attributes"]; has {
		t.Errorf("describe(%q, raw) carries attributes; with a request body the raw "+
			"view is meant to carry schema instead", withBody)
	}
	if _, has := rawDoc["schema"]; !has {
		t.Errorf("describe(%q, raw) omits schema", withBody)
	}
}
