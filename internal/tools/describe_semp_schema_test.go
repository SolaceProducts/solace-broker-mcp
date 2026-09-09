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
	"strings"
	"testing"

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

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	if err := RegisterDescribeSempSchema(server, specs.FS, nil); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}

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

	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	if err := RegisterDescribeSempSchema(server, specs.FS, tm); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}

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
// request whose "arguments" field is present but is not a JSON object — the
// only way to make json.Unmarshal(req.Params.Arguments, &args) itself fail
// inside describe_semp_schema.go's handler, since a normal client call
// always sends a well-formed JSON object for Arguments.
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
// deferred call site in describe_semp_schema.go.
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

// TestDescribeSempSchema_UnparseableArguments_ErrorTypeReachesAuditAndMetric
// covers describe_semp_schema.go's json.Unmarshal-failure branch: the
// "arguments" field is present on the wire but is not a JSON object, so
// json.Unmarshal(req.Params.Arguments, &args) itself fails. This is a
// behavioral check — it drives the branch through a real dispatch and reads
// back the audit/metric surfaces it actually writes — distinct from a static
// scan of error_type literals in the source.
func TestDescribeSempSchema_UnparseableArguments_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf, forceMalformedArguments)

	ctx := context.Background()
	// forceMalformedArguments overwrites Arguments right before send, so the
	// value supplied here is irrelevant to what actually reaches the wire.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: describeSempSchemaToolName}); err == nil {
		t.Fatalf("expected a protocol-level error for unparseable arguments, got nil")
	}

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeBadRequest)
}

// TestDescribeSempSchema_MissingOperation_ErrorTypeReachesAuditAndMetric
// covers describe_semp_schema.go's missing-required-parameter branch: the
// arguments object parses fine but carries no "operation" key.
func TestDescribeSempSchema_MissingOperation_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      describeSempSchemaToolName,
		Arguments: map[string]any{},
	}); err == nil {
		t.Fatalf("expected a protocol-level error for a missing 'operation' parameter, got nil")
	}

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeBadRequest)
}

// TestDescribeSempSchema_InvalidView_ErrorTypeReachesAuditAndMetric covers
// describe_semp_schema.go's invalid-"view"-value branch: operation is present
// but view is neither "trimmed" nor "raw".
func TestDescribeSempSchema_InvalidView_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: describeSempSchemaToolName,
		Arguments: map[string]any{
			"operation": "config/createMsgVpnQueue",
			"view":      "bogus",
		},
	}); err == nil {
		t.Fatalf("expected a protocol-level error for an invalid 'view' parameter, got nil")
	}

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeBadRequest)
}

// TestDescribeSempSchema_UnknownOperation_ErrorTypeReachesAuditAndMetric
// covers describe_semp_schema.go's not-found branch: a well-formed operation
// identifier that isn't in the embedded spec index (reg.describe's error
// return), mirroring TestSempSchemaMap_UnknownOperation's input but through
// the full dispatch path rather than a direct call to describe().
func TestDescribeSempSchema_UnknownOperation_ErrorTypeReachesAuditAndMetric(t *testing.T) {
	var logBuf bytes.Buffer
	session, p := newDescribeSempSchemaSession(t, &logBuf)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: describeSempSchemaToolName,
		Arguments: map[string]any{
			"operation": "config/thisDoesNotExist",
		},
	}); err == nil {
		t.Fatalf("expected a protocol-level error for an unknown operation, got nil")
	}

	assertDescribeSempSchemaErrorType(t, &logBuf, p, metrics.ErrorTypeNotFound)
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
