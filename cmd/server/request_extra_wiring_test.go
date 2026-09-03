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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const postureRequestExtraEnabled = "request extra middleware is installed (always on)"

// TestInstallRequestMiddleware_LogsRequestExtraEnabled pins that production
// wiring always installs the Extra.Header → handler ctx middleware. The SDK
// keeps the middleware chain unexported, so the startup line is the observable
// contract (same approach as tools/list filtering).
//
// It calls installRequestMiddleware, the one place registration order is
// expressed, rather than registering the middleware itself — so dropping the
// registration from production wiring fails here. Config is the auth-disabled
// default, in which the filter registers nothing: this asserts the Extra.Header
// middleware is unconditional.
func TestInstallRequestMiddleware_LogsRequestExtraEnabled(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()
	installRequestMiddleware(newTestServer(), &config.ServerConfig{}, nil, "")

	var found int
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if msg, _ := entry["msg"].(string); msg == postureRequestExtraEnabled {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("found %d %q lines, want 1:\n%s", found, postureRequestExtraEnabled, buf.String())
	}
}

// The two request-scoped records this test observes, named by msg. Neither is
// interesting in itself here — they are simply the emit sites that log through
// ctx, and so reveal which request's correlation ID the ctx is carrying.
const (
	msgToolListFilter            = "tool list filter"
	msgVerifierContractViolation = "internal: TokenInfo.Extra has unexpected type — verifier contract violation"
)

// syncBuffer is a mutex-guarded log sink. The plain bytes.Buffer the other
// wiring tests use is fine when everything logs on the caller's goroutine, but
// this test drives a live SDK session, so a stray write from a transport
// goroutine would otherwise be a data race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestInstallRequestMiddleware_RequestExtraRunsBeforeEveryEmitSite pins that
// auth.RequestExtraMiddleware is registered LAST in installRequestMiddleware,
// which is what makes it outermost and run FIRST.
//
// Why an ordering test and not just the comment: every middleware registered
// inside it logs through the correlation slog handler, which reads the ID off
// the record's ctx. If the Extra.Header copy runs after any of them, that one
// keeps reporting the ID of the POST that opened the session — for the
// session's whole life — while sites below it report the current one. Records
// that disagree about which request they describe are worse than records that
// are uniformly stale, and no unit test of any single middleware can see it.
//
// Both middleware currently registered inside it are checked, by the record
// each one emits, and both are required present so the test cannot pass
// vacuously. Two sites rather than one because they pin different edges:
// tools.WithListFiltering's audit record, and PrincipalMiddleware's
// non-string-claim ERROR. The filter alone would not pin the second — moving
// the copy to sit between the two leaves the filter's record fresh and only
// the ERROR stale, which is the mutation that motivated adding it. The third
// edge, Principal before the filter, is TestPrincipalReachesListFiltering.
//
// This enumerates emit sites, so it does not automatically cover middleware
// added to installRequestMiddleware later. Anything new registered inside the
// Extra.Header copy that logs through ctx needs its record added here.
func TestInstallRequestMiddleware_RequestExtraRunsBeforeEveryEmitSite(t *testing.T) {
	sink := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(correlation.NewSlogHandler(
		slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	defer slog.SetDefault(prev)

	cfg := makeEnabledConfig()
	on := true
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &on

	pool := semp.NewBrokerPool(cfg, nil)
	t.Cleanup(pool.Close)
	mgr := tools.NewToolManager(pool)
	server := newTestServer()
	tools.RegisterWithServer(mgr, server, pool, true, policyFrom(t, cfg), "groups")
	tools.RegisterListBrokers(server, pool)

	// The production wiring function itself, not a copy of its body — the same
	// reason TestPrincipalReachesListFiltering calls it.
	installRequestMiddleware(server, cfg, policyFrom(t, cfg), "groups")

	// jti is deliberately not a string: that is what makes PrincipalMiddleware
	// emit its ERROR, giving this test a second ctx-logging site to observe.
	verifier := func(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
		return &sdkauth.TokenInfo{
			UserID:     "auth0|ordering",
			Expiration: time.Now().Add(time.Hour),
			Extra: map[string]any{
				"iss":                         "https://idp.ordering.example",
				"client_id":                   "ordering-client",
				"jti":                         42,
				authz.TokenInfoExtraKeyGroups: []string{"Ops"},
			},
		}, nil
	}

	// correlation.Middleware is in the chain exactly as production composes it.
	// Without it the session's frozen ctx would carry no ID at all, and a
	// regression would show up as a missing field rather than as the stale ID
	// an operator would actually be handed.
	handler := correlation.Middleware(
		sdkauth.RequireBearerToken(verifier, nil)(
			mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	current := &atomic.Pointer[string]{}
	initializeID, listID := "corr-id-of-initialize", "corr-id-of-tools-list"
	current.Store(&initializeID)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: &correlatedBearer{id: current}},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// From here the POST carries a different ID than the one that opened the
	// session — the whole point of the test.
	current.Store(&listID)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Records are matched by the ID they carry, not by when they were written.
	// A byte offset taken after Connect looks equivalent and is not: the
	// initialized notification's record can land after it, and the test then
	// fails on a record belonging to a POST it never meant to inspect.
	logged := sink.String()
	filterIDs := correlationIDsOf(t, logged, msgToolListFilter)
	principalIDs := correlationIDsOf(t, logged, msgVerifierContractViolation)

	// One tools/list in the whole run, so exactly one filter record, and it
	// must name that request.
	if len(filterIDs) != 1 {
		t.Fatalf("want exactly 1 %q record, got %d:\n%s", msgToolListFilter, len(filterIDs), logged)
	}
	if filterIDs[0] != listID {
		t.Errorf("%q record has correlation_id %q, want %q — it reported the POST that opened the session, so auth.RequestExtraMiddleware did not run before tools.WithListFiltering. It must be registered LAST in installRequestMiddleware.",
			msgToolListFilter, filterIDs[0], listID)
	}

	// Vacuity guard first: with no such record at all the ID assertion below
	// would fail for the wrong reason and point at the wrong culprit.
	if len(principalIDs) == 0 {
		t.Fatalf("no %q record at all; the test proves nothing without it:\n%s",
			msgVerifierContractViolation, logged)
	}
	// PrincipalMiddleware emits on every message, so this asserts on the ID
	// rather than on a count: the tools/list one must name tools/list. If it
	// named the session's first POST instead, none would carry listID.
	if !slices.Contains(principalIDs, listID) {
		t.Errorf("no %q record carries correlation_id %q (saw %q) — auth.RequestExtraMiddleware did not run before auth.PrincipalMiddleware. It must be registered LAST in installRequestMiddleware.",
			msgVerifierContractViolation, listID, principalIDs)
	}
}

// correlationIDsOf returns the correlation_id of every record whose msg is
// exactly want, in order. A record missing the field yields "", which fails
// the caller's comparison with the same meaning as a stale ID: the site did
// not see this request.
func correlationIDsOf(t *testing.T, logged, want string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("non-JSON log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); msg != want {
			continue
		}
		id, _ := rec["correlation_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// correlatedBearer sets a fixed bearer token and a correlation ID the test
// swaps between requests, so one session can present a different ID per POST.
type correlatedBearer struct{ id *atomic.Pointer[string] }

func (c *correlatedBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer ordering-token")
	clone.Header.Set("X-Correlation-ID", *c.id.Load())
	return http.DefaultTransport.RoundTrip(clone)
}

// requestExtraStubHandler is a minimal tools.ToolHandler, standing in for the
// unexported stubHandler used inside internal/tools. handleFn lets the test
// observe whatever the real production middleware chain left on ctx.
type requestExtraStubHandler struct {
	name     string
	handleFn func(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error)
}

func (h *requestExtraStubHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name:        h.name,
		Description: "A test tool",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		Annotations: tools.Annotations{ReadOnly: true},
	}
}

func (h *requestExtraStubHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return h.handleFn(ctx, tc, params)
}

// fixedHeaderTripper sets a fixed set of headers on every outbound request.
type fixedHeaderTripper struct{ headers map[string]string }

func (f *fixedHeaderTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	for k, v := range f.headers {
		clone.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

// disabledCorrelationConfig loads a minimal ServerConfig (auth mode
// "disabled", one broker so the tool manager can resolve it) and forces the
// correlation capability off, regardless of the environment the test runs
// in.
func disabledCorrelationConfig(t *testing.T) *config.ServerConfig {
	t.Helper()
	cfgYAML := "mcp_client_auth:\n  mode: disabled\nbrokers:\n" +
		"  dev:\n    url: http://localhost:8081\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n"
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Observability.CorrelationIDEnabled = false
	return cfg
}

// TestInstallRequestMiddleware_CorrelationDisabled_ClientHeaderNotStamped is
// the regression test for the blocking bug flagged in PR #371 review
// (Andrea, solace-aross): with OBS_CORRELATION_ID_ENABLED off,
// correlation.Middleware is never wired onto the HTTP /mcp endpoint (see
// buildMCPEndpoint), so a client that supplies its own X-Correlation-ID or
// traceparent must not have it stamped onto the JSON-RPC handler ctx by
// auth.RequestExtraMiddleware either — otherwise the documented "capability
// off → no correlation_id anywhere" invariant (asserted in cmd/server/main.go,
// internal/tools/register.go, correlation.Middleware, and
// internal/semp/correlationhdr) is false.
//
// This drives a real request through installRequestMiddleware's production
// wiring — no correlation.Middleware in the HTTP chain, matching what
// buildMCPEndpoint does when the capability is off — and asserts
// correlation.From(ctx) == "" inside the tool handler even though the client
// sent both a traceparent and an X-Correlation-ID.
func TestInstallRequestMiddleware_CorrelationDisabled_ClientHeaderNotStamped(t *testing.T) {
	cfg := disabledCorrelationConfig(t)

	pool := semp.NewBrokerPool(cfg, nil)
	t.Cleanup(pool.Close)
	mgr := tools.NewToolManager(pool)

	var gotCorr string
	var ran bool
	h := &requestExtraStubHandler{name: "correlation-disabled-tool"}
	h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
		ran = true
		gotCorr = correlation.From(ctx)
		return &tools.ToolResult{StructuredContent: map[string]any{"ok": true}}, nil
	}
	mgr.Register(h)

	server := newTestServer()
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")

	// The production wiring function itself. correlationEnabled is derived
	// inside installRequestMiddleware from cfg.Observability, which
	// disabledCorrelationConfig forced off above.
	installRequestMiddleware(server, cfg, nil, "")

	// No correlation.Middleware in the HTTP chain — this models the
	// capability-off path in buildMCPEndpoint exactly.
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(streamable)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL,
		HTTPClient: &http.Client{Transport: &fixedHeaderTripper{headers: map[string]string{
			correlation.HeaderCorrelationID: "client-supplied-corr-id",
			correlation.HeaderTraceparent:   "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		}}},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      h.name,
		Arguments: map[string]any{"broker": "dev"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if !ran {
		t.Fatal("tool handler never ran")
	}
	if gotCorr != "" {
		t.Errorf("correlation.From(ctx) = %q, want empty — the capability is off, so a client-supplied X-Correlation-ID/traceparent must not reach the handler ctx", gotCorr)
	}
}
