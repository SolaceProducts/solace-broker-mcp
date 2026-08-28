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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the wire-level MCP conformance suite (SOL-150761). Everything
// here asserts on bytes a real client would see — HTTP status, headers, SSE
// frames, JSON-RPC envelopes — rather than on Go structs. Struct-level
// assertions pass happily while the wire contract rots underneath them; that
// is the failure mode this suite exists to catch.
//
// The unit-level tests keep their own scope and are deliberately NOT
// duplicated here: composite_schema_test.go owns minLength guardrails on write
// tools, toollist_budget_test.go owns the tools/list token budget, and
// internal/composite/schema_test.go owns per-tool JSON Schema shape. This file
// asserts only what is true of EVERY tool on the wire.
//
// internal/composite/loader_embedded_test.go already checks kebab-case names
// and duplicate names for the tools declared in tools.yaml. The wire check in
// TestToolsList_WireCompleteness is not a duplicate of it: it runs over the
// tools/list response, so it also covers the native SEMPv1 tools and the
// separately-registered list-brokers and describe-semp-schema, none of which
// the loader test can see.

// wireProtocolVersion is the MCP protocol revision this server speaks.
//
// It is the single place the revision is written down: body_limit_test.go and
// session_timeout_test.go build their initialize bodies from it too, so an
// SDK bump changes one constant rather than a scattering of string literals.
//
// It is NOT taken on trust. TestInitialize_ProtocolVersionDriftDetector makes
// the SDK itself choose a version and fails if that choice differs from this
// constant, and TestInitialize_SDKClientNegotiatesWireVersion does the same
// through the SDK's own client. Re-verify this value on every go-sdk bump —
// the SDK's version constants are unexported, so the tests below are the only
// mechanism that can tell you it moved.
const wireProtocolVersion = "2025-11-25"

// unsupportedProtocolVersion is a syntactically well-formed revision string
// that the SDK cannot possibly support: it predates MCP entirely. Sending it
// forces the SDK down its "client asked for something I don't know" branch,
// which returns the SDK's hard-coded fallback revision.
const unsupportedProtocolVersion = "1999-01-01"

// sep2575ProtocolVersion is the SEP-2575 revision. go-sdk v1.7.0 lists it
// among the versions it supports but refuses to negotiate it over initialize,
// so a client asking for it is answered with wireProtocolVersion instead. That
// refusal is what pins this server's advertised revision, and the drift
// detector below probes it directly.
const sep2575ProtocolVersion = "2026-07-28"

// jsonRPCVersion is the only "jsonrpc" value MCP permits on the wire.
const jsonRPCVersion = "2.0"

// JSON-RPC 2.0 reserved error codes asserted below. Named here rather than
// inline so a failure message reads as a contract violation and not as a
// magic number mismatch.
const (
	codeParseError     = -32700
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// initializeBodyFor builds a minimal MCP initialize request for clientName at
// the revision this server speaks. Callers that need a different revision (the
// negotiation tests) build their own body.
func initializeBodyFor(clientName string) string {
	return initializeBodyAt(clientName, wireProtocolVersion)
}

// initializeBodyAt builds an initialize request pinned to an explicit protocol
// revision, so the negotiation tests can drive versions the server may or may
// not support.
func initializeBodyAt(clientName, protocolVersion string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{`+
			`"protocolVersion":%q,"capabilities":{},`+
			`"clientInfo":{"name":%q,"version":"0.0.1"}}}`,
		protocolVersion, clientName)
}

// The *mcp.Server is built once per package run and shared by every test in
// this file. Building it is expensive — sempv2.ParseSpecs, composite.LoadTools
// and JSON Schema compilation cost seconds under -race — and conformanceHandler
// is called dozens of times, including from inside table-test loops.
//
// Sharing is safe: sessions are created per initialize and are independent,
// and no test here mutates server state (no tool is added or removed after
// registration). This is the first cross-test sharing in the package —
// toollist_budget_test.go and composite_schema_test.go each build their own —
// so a test added here that does mutate the server would need its own.
var (
	conformanceServerOnce sync.Once
	conformanceServer     *mcp.Server
)

// conformanceHandler builds the production streamable-HTTP handler over the
// production tool-registration pipeline: registeredServer (see
// toollist_budget_test.go) mirrors main()'s registration steps exactly, and
// newMCPStreamableOptions is the same constructor main() calls. Tests here
// therefore observe the real server's wire behavior, not a stand-in.
//
// A fresh handler is returned every call — it is cheap, and a per-test handler
// keeps session state from leaking between tests — over the shared server.
//
// Write tools are enabled so tools/list covers the full surface; no tool is
// invoked against a broker (see conformanceRequest's note on offline calls).
func conformanceHandler(t *testing.T) *mcp.StreamableHTTPHandler {
	t.Helper()
	conformanceServerOnce.Do(func() {
		conformanceServer = registeredServer(t, true)
	})
	if conformanceServer == nil {
		// registeredServer failed inside the Do above (which ran under a
		// different test). Without this guard every later test would panic on
		// a nil server instead of reporting the real cause.
		t.Fatal("the shared conformance server failed to build; see the earlier registration failure")
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return conformanceServer
	}, newMCPStreamableOptions())
}

// conformanceRequest builds a POST /mcp request with the headers the SDK
// validates. Passing an empty accept or contentType omits that header, which
// is how the header-enforcement tests exercise the SDK's rejection paths.
//
// Note on offline operation: the broker pool creates SEMP clients lazily and
// the tool calls exercised here either resolve no broker (list-brokers) or
// fail input validation before any SEMP request is issued, so nothing in this
// file touches the network.
func conformanceRequest(body, sessionID, accept, contentType string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/mcp", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	return req
}

// standardAccept is what the spec requires a streamable-HTTP client to send on
// POST: it must be prepared for either an immediate JSON response or an SSE
// stream.
const standardAccept = "application/json, text/event-stream"

// postMCP drives one request through the handler with spec-conformant headers
// and returns the recorder, so callers can assert on status, headers and body.
func postMCP(t *testing.T, handler http.Handler, body, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, conformanceRequest(body, sessionID, standardAccept, "application/json"))
	return rec
}

// jsonRPCEnvelope is a deliberately loose view of a JSON-RPC response: every
// field stays raw or generic so the tests can assert the ACTUAL wire types
// (numeric code, string message, presence of keys) instead of letting Go's
// unmarshaler paper over a wrong type. Decoding straight into mcp.Result
// structs would hide exactly the drift this suite is meant to catch.
type jsonRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// assertErrorCode checks error.code on the wire: that it arrived as a bare
// JSON number rather than a quoted string, and that it is the code the
// contract requires.
//
// The quoting check has to look at the raw bytes. Decoding into an int or a
// json.Number would not catch it — encoding/json accepts `"-32602"` into
// either and parses it — so a server that started quoting the code would slip
// through a typed field unnoticed.
func assertErrorCode(t *testing.T, code json.RawMessage, want int) {
	t.Helper()
	if len(code) == 0 {
		t.Fatal("error.code is absent; JSON-RPC 2.0 requires it on every error")
	}
	if code[0] == '"' {
		t.Fatalf("error.code = %s, want a JSON number; a quoted code breaks clients "+
			"that switch on it numerically", code)
	}
	var got int
	if err := json.Unmarshal(code, &got); err != nil {
		t.Fatalf("error.code = %s, want a JSON integer: %v", code, err)
	}
	if got != want {
		t.Errorf("error.code = %d, want %d", got, want)
	}
}

// sseFrame is one parsed text/event-stream frame: the value of its "event:"
// field (empty when unlabelled) and the joined payload of its "data:" fields.
type sseFrame struct {
	event string
	data  string
}

// sseFrames splits a text/event-stream body into its frames. It enforces the
// framing rules rather than merely tolerating them: frames are blank-line
// separated, every line must be a recognised SSE field, and a frame must carry
// data. A body that merely happens to contain the right JSON but is not legal
// SSE fails here.
//
// Line endings are normalised to LF first. The SSE spec accepts CRLF, LF and a
// lone CR interchangeably (go-sdk's writeEvent emits LF today, in event.go),
// so a server switching terminators is still conformant and must not fail this
// suite. Assertions on frame contents therefore go through this parser rather
// than searching the raw body for a literal, which would couple them to the
// terminator the SDK happens to use.
func sseFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	if body == "" {
		t.Fatal("SSE body is empty; the server must emit at least one frame")
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE body does not end with a blank line (frame terminator); got %q", body)
	}
	var frames []sseFrame
	for frame := range strings.SplitSeq(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		var parsed sseFrame
		var data []string
		for line := range strings.SplitSeq(frame, "\n") {
			field, value, ok := strings.Cut(line, ":")
			if !ok {
				t.Fatalf("SSE line %q is not a `field: value` pair", line)
			}
			if field == "" {
				continue // A line starting with ":" is a legal SSE comment.
			}
			switch field {
			case "data":
				data = append(data, strings.TrimPrefix(value, " "))
			case "event":
				parsed.event = strings.TrimPrefix(value, " ")
			case "id", "retry":
				// Legal SSE fields the SDK may emit; nothing to assert.
			default:
				t.Errorf("SSE frame carries unknown field %q in line %q", field, line)
			}
		}
		if len(data) == 0 {
			t.Fatalf("SSE frame %q carries no data field", frame)
		}
		parsed.data = strings.Join(data, "\n")
		frames = append(frames, parsed)
	}
	return frames
}

// decodeSingleResponse asserts the recorder holds exactly one JSON-RPC message
// — as either an SSE stream or a bare JSON body — and returns it. The SDK
// chooses the encoding, so tests that care about the ENVELOPE should not also
// have to care about which framing it arrived in; TestSSE_* below asserts the
// framing itself.
func decodeSingleResponse(t *testing.T, rec *httptest.ResponseRecorder) jsonRPCEnvelope {
	t.Helper()
	payload := rec.Body.String()
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		frames := sseFrames(t, payload)
		if len(frames) != 1 {
			t.Fatalf("got %d SSE frames, want exactly 1; body: %s", len(frames), rec.Body.String())
		}
		payload = frames[0].data
	}
	var env jsonRPCEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("response is not JSON: %v; body: %s", err, rec.Body.String())
	}
	return env
}

// initializeSessionFor performs the initialize handshake and returns the
// session ID, so follow-up requests (tools/list, tools/call) land on an
// initialized session. The stateful handler rejects them otherwise.
func initializeSessionFor(t *testing.T, handler http.Handler) string {
	t.Helper()
	rec := postMCP(t, handler, initializeBodyFor("conformance-test"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize returned no Mcp-Session-Id; the stateful handler must issue one")
	}
	// The spec requires the initialized notification before other requests.
	// The SDK answers a notification with 202 and no body.
	notify := postMCP(t, handler,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, sessionID)
	if notify.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized status = %d, want %d; body: %s",
			notify.Code, http.StatusAccepted, notify.Body.String())
	}
	return sessionID
}

// assertEnvelope checks the parts of the JSON-RPC 2.0 envelope that are
// invariant for every response, success or failure: the version string is
// exactly "2.0", the request's id comes back verbatim, and result/error are
// mutually exclusive. A client that cannot correlate a response to its request
// is broken regardless of what the payload says, which is why the id check is
// a Fatal and not an Error.
func assertEnvelope(t *testing.T, env jsonRPCEnvelope, wantID string) {
	t.Helper()
	if env.JSONRPC != jsonRPCVersion {
		t.Errorf("jsonrpc = %q, want %q", env.JSONRPC, jsonRPCVersion)
	}
	if got := string(env.ID); got != wantID {
		t.Fatalf("id = %s, want %s; the client cannot correlate this response", got, wantID)
	}
	switch {
	case env.Result == nil && env.Error == nil:
		t.Fatal("response carries neither `result` nor `error`; JSON-RPC 2.0 requires exactly one")
	case env.Result != nil && env.Error != nil:
		t.Fatal("response carries both `result` and `error`; JSON-RPC 2.0 permits only one")
	}
}

// TestJSONRPC_SuccessEnvelope pins the success half of the JSON-RPC 2.0
// envelope on the wire: version "2.0", the request id echoed, a `result`
// object and no `error` key at all. Asserting on the raw bytes (rather than on
// a decoded mcp.InitializeResult) is the point — a Go struct round-trip would
// still succeed if the server started emitting `"jsonrpc":"2"` or dropped the
// id, and every MCP client would break.
func TestJSONRPC_SuccessEnvelope(t *testing.T) {
	handler := conformanceHandler(t)

	rec := postMCP(t, handler, initializeBodyFor("conformance-test"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	env := decodeSingleResponse(t, rec)
	assertEnvelope(t, env, "1")
	if env.Error != nil {
		t.Fatalf("initialize returned an error: %s", env.Error.Message)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("`result` is not a JSON object: %v", err)
	}
	for _, key := range []string{"protocolVersion", "capabilities", "serverInfo"} {
		if _, ok := result[key]; !ok {
			t.Errorf("initialize result is missing required field %q; got keys %v", key, keysOf(result))
		}
	}
}

// keysOf lists a map's keys for failure messages, so a missing-field failure
// says what the server DID send rather than only what it did not.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestJSONRPC_ErrorEnvelopeForUnknownTool pins two things at once, because
// one stimulus establishes both.
//
// The first is the error half of the JSON-RPC envelope: version, echoed id,
// `error` and no `result`, a numeric code and a non-empty message.
//
// The second is a conformance rule in its own right. MCP classifies an unknown
// tool name as a protocol error rather than a tool result, because it is not
// something the model can correct from a result body. That is the opposite
// bucket from TestToolsCall_InputValidationIsAToolResult, and pinning both
// directions is what makes the classification meaningful — a server that
// returned isError for everything would pass the validation test alone.
//
// The code is asserted rather than the message text: -32602 is the contract
// (mcp/server.go returns jsonrpc.CodeInvalidParams for an unknown tool),
// whereas the wording is the SDK's and may be reworded on any bump.
func TestJSONRPC_ErrorEnvelopeForUnknownTool(t *testing.T) {
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)

	rec := postMCP(t, handler,
		`{"jsonrpc":"2.0","id":42,"method":"tools/call",`+
			`"params":{"name":"no-such-tool","arguments":{}}}`, sessionID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	env := decodeSingleResponse(t, rec)
	assertEnvelope(t, env, "42")
	if env.Error == nil {
		t.Fatalf("an unknown tool name must be a JSON-RPC error, not a result; got result: %s", env.Result)
	}
	assertErrorCode(t, env.Error.Code, codeInvalidParams)
	if env.Error.Message == "" {
		t.Error("error.message is empty; JSON-RPC 2.0 requires a short description")
	}
	// `data` is optional. When the server does send it, it must be valid JSON
	// — a client that tries to parse a bare string here would fail.
	if env.Error.Data != nil && !json.Valid(env.Error.Data) {
		t.Errorf("error.data is not valid JSON: %s", env.Error.Data)
	}
}

// TestJSONRPC_TransportLevelErrorsBypassTheEnvelope records a deliberate
// deviation rather than a desired behavior. JSON-RPC 2.0 §5.1 says a parse
// error (-32700) and an unknown method (-32601) should come back as JSON-RPC
// error *responses*. go-sdk v1.7.0 instead terminates both at the HTTP layer
// with a plain-text 400 and no envelope, so a client that only knows how to
// read JSON-RPC sees an opaque transport failure.
//
// This is an SDK behavior, not ours — we do not own the dispatch path — and it
// only affects malformed clients, so it is pinned rather than fixed. If an SDK
// upgrade starts returning proper envelopes, this test fails and the finding
// can be closed. See the SOL-150761 report.
func TestJSONRPC_TransportLevelErrorsBypassTheEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		// wantJSONRPCCode is the code JSON-RPC 2.0 would require if the
		// deviation were fixed; it is recorded here so the gap is legible.
		wantJSONRPCCode int
	}{
		{
			name:            "malformed JSON",
			body:            `{"jsonrpc":"2.0","id":1,`,
			wantCode:        http.StatusBadRequest,
			wantJSONRPCCode: codeParseError,
		},
		{
			name:            "unknown method",
			body:            `{"jsonrpc":"2.0","id":1,"method":"no/such/method"}`,
			wantCode:        http.StatusBadRequest,
			wantJSONRPCCode: codeMethodNotFound,
		},
	}
	handler := conformanceHandler(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := initializeSessionFor(t, handler)

			rec := postMCP(t, handler, tc.body, sessionID)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			var env jsonRPCEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err == nil && env.JSONRPC == jsonRPCVersion {
				t.Fatalf("the SDK now returns a JSON-RPC envelope here (body: %s); "+
					"the documented deviation is fixed — update this test to assert "+
					"error.code == %d and drop the finding from SOL-150761",
					rec.Body.String(), tc.wantJSONRPCCode)
			}
		})
	}
}

// initializeProtocolVersion runs one initialize handshake at the requested
// revision and returns the revision the server chose.
func initializeProtocolVersion(t *testing.T, handler http.Handler, requested string) string {
	t.Helper()
	rec := postMCP(t, handler, initializeBodyAt("conformance-test", requested), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize at %q: status = %d, want %d; body: %s",
			requested, rec.Code, http.StatusOK, rec.Body.String())
	}
	env := decodeSingleResponse(t, rec)
	if env.Error != nil {
		t.Fatalf("initialize at %q returned an error: %s", requested, env.Error.Message)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decoding initialize result: %v; body: %s", err, env.Result)
	}
	if result.ProtocolVersion == "" {
		t.Fatal("initialize result has no protocolVersion; the field is mandatory")
	}
	return result.ProtocolVersion
}

// TestInitialize_EchoesSupportedProtocolVersion asserts the negotiation rule a
// client depends on: when it asks for a revision the server supports, it gets
// that exact revision back and may keep speaking it. The 2025-06-18 case is
// not decoration — real clients in the field still pin it, and a regression
// that silently upgraded them to 2025-11-25 would change tool-result and
// elicitation semantics under their feet.
func TestInitialize_EchoesSupportedProtocolVersion(t *testing.T) {
	handler := conformanceHandler(t)
	for _, requested := range []string{wireProtocolVersion, "2025-06-18"} {
		t.Run(requested, func(t *testing.T) {
			if got := initializeProtocolVersion(t, handler, requested); got != requested {
				t.Errorf("protocolVersion = %q, want %q echoed back", got, requested)
			}
		})
	}
}

// TestInitialize_ProtocolVersionDriftDetector is the reason this file exists.
//
// The wire contract is "which MCP revision does this server speak", and it is
// decided entirely inside go-sdk's negotiatedVersion(). That function and its
// version constants are UNEXPORTED, so a test cannot read the answer out of
// the SDK — an SDK bump could move the negotiated revision and every
// hard-coded assertion elsewhere in the tree would keep passing while the wire
// contract changed underneath it.
//
// The fix is to make the SDK answer the question itself, with two probes that
// pin different halves of negotiatedVersion():
//
//   - The cap. A client asking for the SEP-2575 revision is deliberately
//     negotiated DOWN, because negotiatedVersion() refuses to return anything
//     at or above it over the deprecated initialize method. That refusal is
//     what fixes this server at wireProtocolVersion, so relaxing it is the
//     realistic drift — and this is the probe that catches it.
//   - The fallback. A client asking for a revision the SDK does not know at
//     all gets the SDK's hard-coded fallback revision, which today happens to
//     equal the cap. It is a separate constant in the SDK and can move on its
//     own, so it is pinned on its own.
//
// Either failure names both revisions, so the reviewer knows the server's
// advertised protocol changed and can re-verify the contract. Editing
// wireProtocolVersion without an SDK bump fails the same way.
//
// This was preferred over reflecting into the SDK (brittle, and unexported
// package-level constants are not reachable at all) and over asserting a bare
// literal (which is what the drift is). TestInitialize_SDKClientNegotiatesWireVersion
// below cross-checks the same value through the SDK's own client, so the
// contract is confirmed from both ends of the connection.
func TestInitialize_ProtocolVersionDriftDetector(t *testing.T) {
	handler := conformanceHandler(t)

	t.Run("SEP-2575 request is negotiated down", func(t *testing.T) {
		got := initializeProtocolVersion(t, handler, sep2575ProtocolVersion)

		if got != wireProtocolVersion {
			t.Fatalf("a client requesting %q negotiated %q, but this repo declares "+
				"wireProtocolVersion = %q.\n"+
				"The go-sdk's initialize cap has moved: it now negotiates revisions "+
				"it previously refused over initialize. This server's advertised "+
				"protocol has changed — re-verify the contract (docs, README, "+
				"conformance claims), then update wireProtocolVersion.",
				sep2575ProtocolVersion, got, wireProtocolVersion)
		}
	})

	t.Run("unsupported request falls back", func(t *testing.T) {
		got := initializeProtocolVersion(t, handler, unsupportedProtocolVersion)

		if got != wireProtocolVersion {
			t.Fatalf("the SDK now falls back to %q for an unsupported client revision "+
				"(%q), but this repo declares wireProtocolVersion = %q.\n"+
				"The go-sdk's fallback revision has changed. Re-verify the MCP "+
				"revision this server advertises, then update wireProtocolVersion.",
				got, unsupportedProtocolVersion, wireProtocolVersion)
		}
	})
}

// TestInitialize_SDKClientNegotiatesWireVersion drives the SDK's own client
// against the production handler over real HTTP (loopback only — no broker and
// no external network), and asserts both ends land on wireProtocolVersion.
//
// It complements the drift detector above by exercising the path a real agent
// takes, including the SEP-2575 server/discover probe the v1.7.0 client sends
// first. That probe does not fail for want of an answer: the stateful
// transport reports the legacy revisions it supports, and refuses only
// 2026-07-28 and above (StreamableServerTransport.SupportsProtocolVersion
// admits those only when the transport is stateless). The client then rejects
// the result because nothing on offer reaches 2026-07-28, and falls back to
// the legacy initialize handshake — which is what caps this server at
// wireProtocolVersion. If a future SDK makes the stateful transport admit
// 2026-07-28, the client will negotiate it here and this test fails, flagging
// a protocol change the initialize probes above cannot see.
func TestInitialize_SDKClientNegotiatesWireVersion(t *testing.T) {
	srv := httptest.NewServer(conformanceHandler(t))
	defer srv.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "conformance-test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("SDK client failed to connect to the production handler: %v", err)
	}
	defer func() { _ = session.Close() }()

	result := session.InitializeResult()
	if result == nil {
		t.Fatal("client session has no InitializeResult after a successful Connect")
	}
	if result.ProtocolVersion != wireProtocolVersion {
		t.Errorf("SDK client and server negotiated %q, want %q; "+
			"the protocol revision this server speaks has changed",
			result.ProtocolVersion, wireProtocolVersion)
	}
}

// TestSSE_ResponseFraming pins the streamable-HTTP response encoding. A POST
// carrying the spec's Accept header comes back as a text/event-stream whose
// body is well-formed SSE — `field: value` lines grouped into blank-line
// separated frames — with the JSON-RPC response in the data field.
//
// This matters because the framing, not just the payload, is the contract:
// a client's SSE reader will stall or mis-frame if the server stops
// terminating frames with a blank line, folds two responses into one frame, or
// switches the Content-Type. None of that would be visible to a test that
// decoded the body as JSON.
func TestSSE_ResponseFraming(t *testing.T) {
	handler := conformanceHandler(t)

	rec := postMCP(t, handler, initializeBodyFor("conformance-test"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	// The media type must be text/event-stream; a charset parameter is
	// permitted, so compare the base type rather than the whole header.
	contentType := rec.Header().Get("Content-Type")
	if base, _, _ := strings.Cut(contentType, ";"); strings.TrimSpace(base) != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}

	frames := sseFrames(t, rec.Body.String())
	if len(frames) != 1 {
		t.Fatalf("got %d SSE frames for a single request, want 1; body: %s",
			len(frames), rec.Body.String())
	}
	// The SDK labels response frames `event: message`. Assert it explicitly:
	// clients that dispatch on the event name ignore frames of other types.
	// Read via the parser rather than searching the raw body, so the assertion
	// is not coupled to the line terminator the SDK emits.
	if frames[0].event != "message" {
		t.Errorf("SSE frame is labelled %q, want `message`; body: %s",
			frames[0].event, rec.Body.String())
	}
	var env jsonRPCEnvelope
	if err := json.Unmarshal([]byte(frames[0].data), &env); err != nil {
		t.Fatalf("SSE data field is not the JSON-RPC response: %v; data: %s", err, frames[0].data)
	}
	assertEnvelope(t, env, "1")
}

// TestStreamableHTTP_AcceptHeaderEnforcement pins the SDK's precondition
// checks on POST. The spec requires a client to accept BOTH application/json
// and text/event-stream, because the server picks the encoding. A client that
// advertises only one gets 400 before any MCP method runs.
//
// Pinning the rejection matters as much as pinning the success: these checks
// run ahead of session lookup and tool dispatch, so a regression that relaxed
// them would let a half-capable client open a session it cannot read from.
func TestStreamableHTTP_AcceptHeaderEnforcement(t *testing.T) {
	tests := []struct {
		name       string
		accept     string
		wantStatus int
	}{
		{"both media types", standardAccept, http.StatusOK},
		{"wildcard", "*/*", http.StatusOK},
		{"json only", "application/json", http.StatusBadRequest},
		{"event-stream only", "text/event-stream", http.StatusBadRequest},
		{"absent", "", http.StatusBadRequest},
	}
	handler := conformanceHandler(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, conformanceRequest(
				initializeBodyFor("conformance-test"), "", tc.accept, "application/json"))

			if rec.Code != tc.wantStatus {
				t.Errorf("Accept %q: status = %d, want %d; body: %s",
					tc.accept, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestStreamableHTTP_ContentTypeEnforcement pins the other precondition: a
// POST body must be declared application/json, and anything else — including
// no Content-Type at all — is 415 Unsupported Media Type, not 400. The
// distinction is load-bearing for clients that retry on 4xx: 415 says "resend
// with the right header", 400 says "your request was wrong".
func TestStreamableHTTP_ContentTypeEnforcement(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{"application/json", "application/json", http.StatusOK},
		{"with charset", "application/json; charset=utf-8", http.StatusOK},
		{"text/plain", "text/plain", http.StatusUnsupportedMediaType},
		{"absent", "", http.StatusUnsupportedMediaType},
	}
	handler := conformanceHandler(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, conformanceRequest(
				initializeBodyFor("conformance-test"), "", standardAccept, tc.contentType))

			if rec.Code != tc.wantStatus {
				t.Errorf("Content-Type %q: status = %d, want %d; body: %s",
					tc.contentType, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// wireTool is the on-the-wire shape of a tools/list entry. Every field is
// raw or a pointer so the test can distinguish "absent" from "present but
// empty" — the whole point of a completeness check.
type wireTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  json.RawMessage `json:"annotations"`
}

// listToolsOverWire drives tools/list through the HTTP handler, following
// pagination to completion, and returns the raw tool entries. It reads the
// bytes the client receives rather than the SDK's decoded structs, so a field
// the server stops emitting is visible here.
func listToolsOverWire(t *testing.T, handler http.Handler, sessionID string) []wireTool {
	t.Helper()
	// A server that returned a constant nextCursor would loop here until the
	// package-level test timeout killed the run with an opaque panic. The cap
	// is far above the real page count (one page today) and fails legibly.
	const maxPages = 50
	var all []wireTool
	cursor := ""
	for id := 100; id < 100+maxPages; id++ {
		params := "{}"
		if cursor != "" {
			params = fmt.Sprintf("{%q:%q}", "cursor", cursor)
		}
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":%s}`, id, params)
		rec := postMCP(t, handler, body, sessionID)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		env := decodeSingleResponse(t, rec)
		if env.Error != nil {
			t.Fatalf("tools/list returned an error: %s", env.Error.Message)
		}
		var result struct {
			Tools      []wireTool `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(env.Result, &result); err != nil {
			t.Fatalf("decoding tools/list result: %v", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all
		}
		cursor = result.NextCursor
	}
	t.Fatalf("tools/list still returned a nextCursor after %d pages (%d tools); "+
		"pagination is not terminating", maxPages, len(all))
	return nil
}

// TestToolsList_WireCompleteness asserts the invariants that must hold for
// EVERY tool the server exposes, on the wire, whatever mechanism defined it
// (composite YAML, native SEMPv1, or the separately-registered list-brokers
// and describe-semp-schema).
//
// An LLM only ever sees these bytes. A tool with no description, or with an
// input schema the client cannot read, is unusable no matter how correct its
// implementation is — and nothing else in the tree checks the whole surface at
// once: composite_schema_test.go checks minLength on write-tool params,
// toollist_budget_test.go checks the total token cost, and
// internal/composite/schema_test.go checks individual generated schemas.
// This is the only test that says "no tool may be missing these".
func TestToolsList_WireCompleteness(t *testing.T) {
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)

	tools := listToolsOverWire(t, handler, sessionID)
	if len(tools) == 0 {
		t.Fatal("tools/list returned no tools; the registration pipeline is broken")
	}

	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool.Name == "" {
			t.Fatalf("a tool entry has an empty name: %+v", tool)
		}
		t.Run(tool.Name, func(t *testing.T) {
			if seen[tool.Name] {
				t.Fatalf("tool %q appears twice in tools/list", tool.Name)
			}
			seen[tool.Name] = true

			// House rule (CLAUDE.md): every MCP tool name is kebab-case. LLMs
			// pattern-match on this string, so a stray snake_case name is a
			// real usability regression, not a style nit.
			if !isKebabCase(tool.Name) {
				t.Errorf("tool name %q is not kebab-case", tool.Name)
			}
			if strings.TrimSpace(tool.Description) == "" {
				t.Error("description is empty; the model has nothing to select this tool on")
			}
			assertObjectSchema(t, "inputSchema", tool.InputSchema)

			// Output schemas: every tool that flows through ToolManager
			// declares one, and the manager validates its result against it
			// before the result reaches the wire. The exception below is a
			// known gap, pinned so it cannot spread silently.
			if toolsWithoutOutputSchema[tool.Name] {
				if tool.OutputSchema != nil {
					t.Errorf("%s now declares an outputSchema; "+
						"remove it from toolsWithoutOutputSchema", tool.Name)
				}
			} else {
				assertObjectSchema(t, "outputSchema", tool.OutputSchema)
			}
			if tool.Annotations == nil {
				t.Error("annotations are absent; clients use readOnlyHint to decide " +
					"whether a call needs confirmation")
				return
			}
			var annotations map[string]any
			if err := json.Unmarshal(tool.Annotations, &annotations); err != nil {
				t.Fatalf("annotations are not a JSON object: %v", err)
			}
			if _, ok := annotations["readOnlyHint"]; !ok {
				t.Errorf("annotations omit readOnlyHint; got keys %v", keysOf(annotations))
			}
		})
	}
}

// assertObjectSchema checks that a schema field is present and is a JSON
// Schema object declaring `"type": "object"` — the only root type MCP permits
// for a tool input schema, and what every tool here uses for output too.
func assertObjectSchema(t *testing.T, field string, raw json.RawMessage) {
	t.Helper()
	if raw == nil {
		t.Errorf("%s is absent", field)
		return
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Errorf("%s is not a JSON object: %v", field, err)
		return
	}
	if schema["type"] != "object" {
		t.Errorf(`%s has type %v, want "object"`, field, schema["type"])
	}
}

// isKebabCase reports whether name is lower-case alphanumerics separated by
// single hyphens, with no leading or trailing hyphen.
func isKebabCase(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") ||
		strings.Contains(name, "--") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// toolsWithoutOutputSchema records the tools that return structuredContent but
// declare no outputSchema, so a client cannot validate what it gets back. MCP
// 2025-11-25 says a tool returning structured content SHOULD declare an output
// schema; these do not.
//
// describe-semp-schema (internal/tools/describe_semp_schema.go) is registered
// outside the ToolManager pipeline, which is where every other tool's output
// schema is attached and enforced — the gap is a consequence of that bypass,
// not a deliberate choice.
//
// This is an allow-list, not an excuse: TestToolsList_WireCompleteness fails
// if a tool listed here starts declaring a schema (remove it from the map) and
// fails if any tool NOT listed here stops declaring one. Closing the gap is a
// production change and is tracked separately — see the SOL-150761 report.
var toolsWithoutOutputSchema = map[string]bool{
	"describe-semp-schema": true,
}

// wireToolResult is the on-the-wire shape of a tools/call result. IsError is a
// pointer so "absent" is distinguishable from "false": MCP treats the two
// identically for a successful call, but the input-validation tests below need
// to prove the field was actually sent.
type wireToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
	IsError           *bool             `json:"isError"`
}

// callToolOverWire issues one tools/call and returns the raw JSON-RPC envelope,
// so callers can assert whether the failure mode is a tool result or a
// protocol error — the distinction the input-validation tests turn on.
func callToolOverWire(t *testing.T, handler http.Handler, sessionID, name, arguments string) jsonRPCEnvelope {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		name, arguments)
	rec := postMCP(t, handler, body, sessionID)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	env := decodeSingleResponse(t, rec)
	assertEnvelope(t, env, "7")
	return env
}

// TestToolsCall_ReturnsStructuredContent pins the successful-call contract:
// the result carries structuredContent (the machine-readable payload an agent
// consumes) alongside the human-readable content blocks, and does not set
// isError.
//
// list-brokers is the subject because it is the one tool that can be called
// offline: it resolves no broker and issues no SEMP request, so the assertion
// is about the wire shape and never about broker availability.
//
// That makes it a single-tool assertion, not a generalisation. list-brokers is
// registered by tools.RegisterListBrokers and builds its own CallToolResult,
// so it is precisely a tool that does NOT go through ToolManager.CallTool's
// result-assembly tail (the same bypass that costs it an output schema — see
// toolsWithoutOutputSchema below). The ToolManager success tail is covered at
// unit level in internal/tools/.
func TestToolsCall_ReturnsStructuredContent(t *testing.T) {
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)

	env := callToolOverWire(t, handler, sessionID, "list-brokers", "{}")
	if env.Error != nil {
		t.Fatalf("list-brokers returned a JSON-RPC error: %s", env.Error.Message)
	}

	var result wireToolResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decoding tools/call result: %v; result: %s", err, env.Result)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("list-brokers reported isError; result: %s", env.Result)
	}
	if result.StructuredContent == nil {
		t.Fatal("result has no structuredContent; agents parse this field, not the text block")
	}
	var structured map[string]any
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
		t.Fatalf("structuredContent is not a JSON object: %v", err)
	}
	if _, ok := structured["brokers"]; !ok {
		t.Errorf("structuredContent does not match the declared outputSchema "+
			"(no `brokers` key); got %v", keysOf(structured))
	}
	// The spec requires content blocks too, for clients that render text.
	if len(result.Content) == 0 {
		t.Error("result has no content blocks; a text fallback is required alongside structuredContent")
	}
}

// TestToolsCall_InputValidationIsAToolResult settles the question SOL-150761
// raised: when a client sends schema-invalid arguments, does this server
// answer with a JSON-RPC error (-32602) or with a tool result carrying
// isError?
//
// MCP 2025-11-25 is explicit that input-validation failures belong in the
// tool-result bucket, not the protocol-error bucket, so the model can see what
// it got wrong and retry. Protocol errors are reserved for things the model
// cannot act on — an unknown tool name, a malformed request.
//
// The measured answer, FOR TOOLS THAT GO THROUGH ToolManager, is: a tool
// result with isError:true, which is conformant. The credit goes to
// ToolManager.CallTool (internal/tools/manager.go), which validates arguments
// against the tool's compiled input schema and returns buildLocalErrorResult —
// NOT to the SDK. Production registers every tool with the untyped
// Server.AddTool, whose contract is explicitly "unmarshaling the arguments and
// validating them against the input schema are the caller's responsibility":
// the SDK performs no input validation on this path at all. The generic
// mcp.AddTool wrapper, which does validate, is not used here.
//
// That makes this test load-bearing in a way it would not be if the SDK owned
// the behavior, and it makes the scope of the claim matter: the answer holds
// only where ToolManager runs. The tools registered outside it do NOT behave
// this way, which TestToolsCall_ValidationOutsideToolManagerIsNotConformant
// pins separately. The two subtests below cover both JSON Schema failure kinds
// a client actually hits on the conformant path.
func TestToolsCall_InputValidationIsAToolResult(t *testing.T) {
	tests := []struct {
		name string
		// arguments are schema-invalid for get-vpn-status, a read-only
		// composite tool whose only required parameter (besides the injected
		// broker) is the string msgVpnName. Validation runs before the SEMP
		// request, so these calls never touch the network.
		arguments string
	}{
		{
			name:      "wrong type",
			arguments: `{"broker":"dev","msgVpnName":42}`,
		},
		{
			name:      "missing required field",
			arguments: `{"broker":"dev"}`,
		},
	}
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := callToolOverWire(t, handler, sessionID, "get-vpn-status", tc.arguments)

			if env.Error != nil {
				t.Fatalf("input validation came back as a JSON-RPC error (code %s: %s).\n"+
					"MCP 2025-11-25 requires input-validation failures to be tool "+
					"results with isError:true so the model can correct itself. "+
					"This is a conformance regression — see SOL-150761.",
					env.Error.Code, env.Error.Message)
			}
			var result wireToolResult
			if err := json.Unmarshal(env.Result, &result); err != nil {
				t.Fatalf("decoding tools/call result: %v; result: %s", err, env.Result)
			}
			if result.IsError == nil || !*result.IsError {
				t.Fatalf("result does not set isError:true for invalid input; "+
					"the model would treat this failure as a successful call. result: %s",
					env.Result)
			}
			if len(result.Content) == 0 {
				t.Fatal("isError result carries no content; the model has nothing to read")
			}
			// The message must say enough for a model to correct itself: our
			// own "parameter validation failed" prefix plus the offending
			// parameter name. The rest of the sentence is xeipuuv/gojsonschema
			// wording, which is a third-party string and not our contract, so
			// it is deliberately not asserted.
			for _, want := range []string{"parameter validation failed", "msgVpnName"} {
				if !strings.Contains(string(env.Result), want) {
					t.Errorf("validation message does not mention %q; result: %s", want, env.Result)
				}
			}
			// The error result is itself structured, so an agent can branch on
			// retryable without parsing prose.
			var structured struct {
				Error     string `json:"error"`
				Retryable *bool  `json:"retryable"`
			}
			if result.StructuredContent == nil {
				t.Fatal("isError result has no structuredContent")
			}
			if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
				t.Fatalf("structuredContent is not the error shape: %v", err)
			}
			if structured.Error == "" {
				t.Error("structuredContent.error is empty")
			}
			if structured.Retryable == nil {
				t.Error("structuredContent.retryable is absent; an agent cannot tell " +
					"a permanent input error from a transient one")
			} else if *structured.Retryable {
				t.Error("structuredContent.retryable = true, want false; " +
					"retrying the same invalid arguments cannot succeed")
			}
		})
	}
}

// TestToolsCall_ValidationOutsideToolManagerIsNotConformant pins the other
// half of the SOL-150761 question, and the answer here is NOT conformant.
//
// Both tools registered outside the ToolManager pipeline get their arguments
// straight from the untyped Server.AddTool handler, with nobody validating
// them against the declared input schema. The two of them fail in opposite
// directions, and both are wrong:
//
//   - describe-semp-schema hand-checks its own arguments and returns a Go
//     error, which the SDK turns into a JSON-RPC error. MCP 2025-11-25 puts
//     input-validation failures in the tool-result bucket, so the model is
//     handed a protocol error it cannot act on. The code makes it worse: a
//     plain error from an untyped handler serialises as `"code":0`, which is
//     not a JSON-RPC error code at all.
//   - list-brokers validates nothing, so an argument that its schema does not
//     declare is silently accepted.
//
// Closing either gap is a production change, tracked separately (SOL-150761).
// Until then this test is a two-way gate, in the same shape as
// toolsWithoutOutputSchema: it fails if the behaviour drifts further AND it
// fails once production is fixed, so whoever fixes it is told to delete the
// case rather than leaving a stale allow-list behind.
func TestToolsCall_ValidationOutsideToolManagerIsNotConformant(t *testing.T) {
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)

	// Known-nonconformant: schema-invalid arguments answered with a JSON-RPC
	// error instead of an isError tool result.
	validationAsProtocolError := []struct {
		name      string
		tool      string
		arguments string
		// wantMessage is our own wording (internal/tools/), not a third-party
		// validator's, so it is safe to assert on.
		wantMessage string
	}{
		{
			name:        "describe-semp-schema missing required field",
			tool:        "describe-semp-schema",
			arguments:   `{}`,
			wantMessage: "missing required parameter 'operation'",
		},
		{
			// A wrong-typed parameter is reported as MISSING, not as
			// wrong-typed. describe_semp_schema.go:354 does
			//
			//	operation, _ := args["operation"].(string)
			//
			// and discards the comma-ok, so a non-string yields "" — which the
			// next line cannot tell apart from an absent key. The caller sent
			// "operation" and is told it did not.
			//
			// That is worse than the misclassification this test is named for.
			// A JSON-RPC error at least tells the model something failed; this
			// tells it the wrong thing, sending it to re-supply a parameter it
			// already supplied — plausibly the same way, so it does not
			// converge. Pinned separately from the missing-field case above
			// because the two are distinct defects that today share a message.
			name:        "describe-semp-schema wrong type is misreported as missing",
			tool:        "describe-semp-schema",
			arguments:   `{"operation":42}`,
			wantMessage: "missing required parameter 'operation'",
		},
	}
	for _, tc := range validationAsProtocolError {
		t.Run(tc.name, func(t *testing.T) {
			env := callToolOverWire(t, handler, sessionID, tc.tool, tc.arguments)

			if env.Error == nil {
				t.Fatalf("%s now answers invalid input with a tool result, not a "+
					"JSON-RPC error — the SOL-150761 conformance gap is CLOSED.\n"+
					"Delete this case and, if it is the last one, this whole test. "+
					"result: %s", tc.tool, env.Result)
			}
			if !strings.Contains(env.Error.Message, tc.wantMessage) {
				t.Errorf("error.message = %q, want it to contain %q",
					env.Error.Message, tc.wantMessage)
			}
			// Pinned, not endorsed: 0 is not a JSON-RPC error code. It is what
			// the SDK emits for a bare Go error from an untyped handler. If
			// this ever becomes a real code the gap has been worked on and the
			// classification above should be re-checked.
			assertErrorCode(t, env.Error.Code, 0)
		})
	}

	// Known-nonconformant: an argument the tool's input schema does not
	// declare is accepted without complaint.
	t.Run("list-brokers accepts undeclared arguments", func(t *testing.T) {
		env := callToolOverWire(t, handler, sessionID, "list-brokers", `{"bogus":1}`)

		if env.Error != nil {
			t.Fatalf("list-brokers now rejects an undeclared argument (code %s: %s) — "+
				"it is being validated. Delete this case from SOL-150761.",
				env.Error.Code, env.Error.Message)
		}
		var result wireToolResult
		if err := json.Unmarshal(env.Result, &result); err != nil {
			t.Fatalf("decoding tools/call result: %v; result: %s", err, env.Result)
		}
		if result.IsError != nil && *result.IsError {
			t.Fatalf("list-brokers now returns isError for an undeclared argument — "+
				"it is being validated. Delete this case from SOL-150761. result: %s",
				env.Result)
		}
	})
}

// toolsWithoutInputValidation records the tools that do NOT validate their
// arguments against their own declared input schema, because they are
// registered outside the ToolManager pipeline (tools.RegisterListBrokers and
// tools.RegisterDescribeSempSchema call mcp.Server.AddTool directly). The
// untyped AddTool contract puts unmarshalling and schema validation on the
// caller, and neither of these two does it — see
// TestToolsCall_ValidationOutsideToolManagerIsNotConformant for the exact
// failure mode of each. Tracked as SOL-153693.
//
// This is an allow-list, not an excuse. TestToolsCall_EveryToolValidatesItsInput
// is a two-way gate over it, in the same shape as toolsWithoutOutputSchema:
// a tool NOT listed here that accepts schema-invalid arguments fails the test
// (the point of the gate — a third tool must not join these two unnoticed),
// and a tool listed here that starts validating also fails it, so whoever
// fixes SOL-153693 is told to delete the entry rather than leave a stale
// allow-list behind.
//
// One caveat, deliberately not papered over: list-brokers declares no
// properties at all, so no schema-invalid argument set can be derived from its
// schema and the gate can only skip it. Its entry is still load-bearing —
// every name here must match a registered tool, so the entry cannot rot after
// a rename — but the "started validating" direction cannot fire for it until
// its schema declares something to violate.
var toolsWithoutInputValidation = map[string]bool{
	"list-brokers":         true,
	"describe-semp-schema": true,
}

// TestToolsCall_EveryToolValidatesItsInput is a gate, not a behaviour test: it
// fails when a newly registered tool accepts schema-invalid arguments.
//
// Input validation on this server lives in ToolManager.CallTool
// (internal/tools/manager.go), which checks arguments against the tool's
// compiled input schema before dispatching to the handler. It does NOT live in
// the SDK: production registers every tool with the untyped mcp.Server.AddTool,
// whose contract explicitly leaves validation to the caller. So a tool
// registered outside ToolManager silently gets none — which is exactly how the
// two tools in toolsWithoutInputValidation ended up unvalidated (SOL-153693).
//
// This test does not fix those two. It stops a third joining them unnoticed:
// every tool on the wire whose schema allows an invalid probe to be derived is
// called with arguments derived from its OWN input schema (see
// deriveInvalidArguments; the tools that allow none are logged and counted),
// and must answer with a tool result carrying isError:true — the
// MCP 2025-11-25 classification for an input-validation failure, which the
// model can read and correct itself from, as opposed to a JSON-RPC error.
//
// No broker is contacted. Validation runs before handler.Handle, and the
// broker alias sent is "dev" from budgetTestConfig, which resolves offline
// (the pool builds SEMP clients lazily) — a bogus alias would fail broker
// resolution first and this test would then be asserting the wrong thing.
func TestToolsCall_EveryToolValidatesItsInput(t *testing.T) {
	handler := conformanceHandler(t)
	sessionID := initializeSessionFor(t, handler)

	allTools := listToolsOverWire(t, handler, sessionID)
	if len(allTools) == 0 {
		t.Fatal("tools/list returned no tools; the registration pipeline is broken")
	}

	registered := make(map[string]bool, len(allTools))
	probed, skipped := 0, 0
	for _, tool := range allTools {
		registered[tool.Name] = true

		arguments, victim, ok := deriveInvalidArguments(t, tool.InputSchema)
		if !ok {
			// Not a pass. A gate that quietly probes nothing is worse than no
			// gate, so every skip is named and the total is reported below.
			skipped++
			t.Logf("SKIP %s: no schema-invalid probe can be derived — its input schema "+
				"declares no required string property other than the injected broker",
				tool.Name)
			continue
		}
		probed++

		t.Run(tool.Name, func(t *testing.T) {
			env := callToolOverWire(t, handler, sessionID, tool.Name, arguments)
			validated := isErrorToolResult(t, env)

			if toolsWithoutInputValidation[tool.Name] {
				if validated {
					t.Fatalf("%s now returns an isError tool result for a schema-invalid %q — "+
						"it is validating its input. The SOL-153693 gap is closed for this "+
						"tool: delete its entry from toolsWithoutInputValidation.", tool.Name, victim)
				}
				return
			}
			if !validated {
				t.Fatalf("%s accepted a schema-invalid %q (an integer where its own input "+
					"schema declares a string) without returning isError:true.\n"+
					"arguments: %s\nresponse: %s\n"+
					"Every tool must validate its arguments against its declared input "+
					"schema. The SDK does not do this for untyped Server.AddTool "+
					"registrations — ToolManager.CallTool does, so a tool registered "+
					"outside that pipeline gets no validation at all (SOL-153693). "+
					"Route the tool through ToolManager, or validate in its handler and "+
					"return an isError result.",
					tool.Name, victim, arguments, responseForLog(env))
			}
		})
	}

	// Staleness guard for the allow-list itself: an entry naming a tool that no
	// longer exists (renamed, removed) would otherwise sit here forever,
	// silently excusing nothing.
	for name := range toolsWithoutInputValidation {
		if !registered[name] {
			t.Errorf("toolsWithoutInputValidation names %q, which is not a registered "+
				"tool; remove the stale entry", name)
		}
	}

	t.Logf("input-validation gate: %d tools probed, %d skipped (of %d registered)",
		probed, skipped, len(allTools))
	if probed == 0 {
		t.Fatal("the gate probed no tools at all; the probe derivation is broken")
	}
}

// isErrorToolResult reports whether a tools/call response is a tool result
// with isError:true — MCP's classification for an input-validation failure.
// A JSON-RPC error is deliberately NOT counted as validation: the model cannot
// act on a protocol error, and describe-semp-schema's rejection-by-protocol-
// error is precisely one of the gaps this gate exists to keep from spreading.
func isErrorToolResult(t *testing.T, env jsonRPCEnvelope) bool {
	t.Helper()
	if env.Error != nil {
		return false
	}
	var result wireToolResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decoding tools/call result: %v; result: %s", err, env.Result)
	}
	return result.IsError != nil && *result.IsError
}

// responseForLog renders whichever half of the envelope arrived, so a failure
// message shows what actually came back rather than an empty Result.
func responseForLog(env jsonRPCEnvelope) string {
	if env.Error != nil {
		return fmt.Sprintf("JSON-RPC error (code %s): %s", env.Error.Code, env.Error.Message)
	}
	return string(env.Result)
}

// deriveInvalidArguments builds one schema-invalid argument set for a tool from
// nothing but that tool's own input schema, so the gate carries no per-tool
// knowledge and covers any tool added later for free.
//
// The single violation introduced is a type error: a required property the
// schema declares as a string is sent as an integer. Every other required
// property is filled with a plausible valid value, so a tool that rejects the
// call is rejecting the one violation and not something incidental.
//
// The injected broker parameter is never the victim. It is read out and
// stripped by ToolManager.CallTool BEFORE validation runs, so a non-string
// broker would be answered with a broker-resolution error instead of a
// validation error — a pass for the wrong reason. It is always sent as "dev".
//
// Returns ok=false when the schema declares no required string property other
// than broker, which is the only case the caller may skip.
func deriveInvalidArguments(t *testing.T, raw json.RawMessage) (arguments, victim string, ok bool) {
	t.Helper()
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decoding inputSchema: %v; schema: %s", err, raw)
	}

	// Sorted so the chosen victim is stable across runs — Go map iteration is
	// not, and a gate that probes a different parameter each run reports
	// failures that are hard to reproduce.
	required := append([]string(nil), schema.Required...)
	sort.Strings(required)

	for _, name := range required {
		if name == brokerParamName {
			continue
		}
		if declaredType(t, schema.Properties[name]) == "string" {
			victim = name
			break
		}
	}
	if victim == "" {
		return "", "", false
	}

	args := make(map[string]any, len(required))
	for _, name := range required {
		switch {
		case name == brokerParamName:
			args[name] = validBrokerAlias
		case name == victim:
			// The one violation: an integer where a string is declared.
			args[name] = 42
		default:
			args[name] = plausibleValue(t, schema.Properties[name])
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encoding probe arguments: %v", err)
	}
	return string(encoded), victim, true
}

const (
	// brokerParamName is the parameter injectBrokerParam adds to every
	// ToolManager-registered tool's schema (internal/tools/register.go).
	brokerParamName = "broker"
	// validBrokerAlias is the only alias in budgetTestConfig. It must resolve,
	// because broker resolution precedes input validation.
	validBrokerAlias = "dev"
)

// declaredType returns a property schema's "type" keyword, or "" if the
// property is absent or declares no single type.
func declaredType(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if raw == nil {
		return ""
	}
	var prop struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &prop); err != nil {
		// A union type ("type": ["string","null"]) lands here. Treat it as
		// untyped rather than failing: it is simply not a usable probe target.
		return ""
	}
	return prop.Type
}

// plausibleValue produces a value that satisfies a property schema, so the
// probe's only schema violation is the one deriveInvalidArguments introduced
// deliberately. It covers the JSON Schema types this server's tools actually
// declare; an enum is honoured because an arbitrary string would violate it.
func plausibleValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var prop struct {
		Type string `json:"type"`
		Enum []any  `json:"enum"`
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &prop); err != nil {
			t.Fatalf("decoding property schema: %v; schema: %s", err, raw)
		}
	}
	if len(prop.Enum) > 0 {
		return prop.Enum[0]
	}
	switch prop.Type {
	case "string":
		// Non-empty, so a minLength:1 constraint (which every named string
		// parameter in tools.yaml carries) is satisfied.
		return "conformance-probe"
	case "integer", "number":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	default:
		// "object" and anything undeclared. An empty object satisfies a schema
		// with no required sub-properties, which is what the *Config parameters
		// on the update tools declare.
		return map[string]any{}
	}
}
