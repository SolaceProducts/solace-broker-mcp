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
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerTripper copies a caller-controlled header set onto every outbound
// request. The test mutates the set between Connect and each CallTool so
// initialize and later tools/call POSTs on one session carry different values.
type headerTripper struct {
	base http.RoundTripper
	mu   sync.Mutex
	hdr  http.Header
}

func (t *headerTripper) set(key, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hdr == nil {
		t.hdr = make(http.Header)
	}
	t.hdr.Set(key, value)
}

func (t *headerTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	t.mu.Lock()
	for k, vs := range t.hdr {
		clone.Header[k] = append([]string(nil), vs...)
	}
	t.mu.Unlock()
	if extra, ok := req.Context().Value(tripperHeadersKey{}).(http.Header); ok {
		for k, vs := range extra {
			clone.Header[k] = append([]string(nil), vs...)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// tripperHeadersKey overlays per-CallTool headers onto the shared headerTripper
// without mutating its set. Concurrent CallTools on one session each pass a
// distinct overlay on their CallTool ctx; the SDK puts that ctx on the HTTP POST.
type tripperHeadersKey struct{}

func withTripperHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, tripperHeadersKey{}, h.Clone())
}

// inboundRecorder stashes the last inbound header value seen on an HTTP POST
// so a test can prove the tools/call POST actually carried the value the
// client was told to send — distinct from what the tool handler reads off ctx.
type inboundRecorder struct {
	key  string
	mu   sync.Mutex
	last string
}

func (r *inboundRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			r.mu.Lock()
			r.last = req.Header.Get(r.key)
			r.mu.Unlock()
		}
		next.ServeHTTP(w, req)
	})
}

func (r *inboundRecorder) lastValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func registerFreshnessTool(t *testing.T, h *metaStubHandler) *mcp.Server {
	t.Helper()
	pool := metaTestPool(t)
	mgr := tools.NewToolManager(pool)
	mgr.Register(h)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	server.AddReceivingMiddleware(auth.RequestExtraMiddleware())
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")
	return server
}

// TestHandlerContext_CorrelationIDFollowsCurrentRequest pins that a tool
// handler's correlation.From(ctx) equals the X-Correlation-ID on THIS
// tools/call POST, not the ID that opened the session.
//
// Two CallTools on one streamable-HTTP session send distinct IDs. The HTTP
// layer must observe each POST's ID (harness check). The handler ctx must
// match that POST, so the two handler observations are different.
func TestHandlerContext_CorrelationIDFollowsCurrentRequest(t *testing.T) {
	t.Parallel()

	const (
		idInit  = "corr-session-init"
		idCall1 = "corr-tools-call-1"
		idCall2 = "corr-tools-call-2"
	)

	var (
		mu   sync.Mutex
		seen []string
	)
	h := &metaStubHandler{name: "corr-freshness-tool"}
	h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
		mu.Lock()
		seen = append(seen, correlation.From(ctx))
		mu.Unlock()
		return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}

	server := registerFreshnessTool(t, h)

	inbound := &inboundRecorder{key: correlation.HeaderCorrelationID}
	var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler = correlation.Middleware(handler)
	handler = inbound.wrap(handler)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	tripper := &headerTripper{}
	httpClient := ts.Client()
	tripper.base = httpClient.Transport
	httpClient.Transport = tripper

	tripper.set(correlation.HeaderCorrelationID, idInit)
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	call := func(id string) {
		t.Helper()
		tripper.set(correlation.HeaderCorrelationID, id)
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      h.name,
			Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
		})
		if err != nil {
			t.Fatalf("CallTool (id=%s): %v", id, err)
		}
		if got := inbound.lastValue(); got != id {
			t.Fatalf("HTTP POST carried %s=%q, want %q (client header did not reach the server)", correlation.HeaderCorrelationID, got, id)
		}
	}

	call(idCall1)
	call(idCall2)

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("handler ran %d time(s), want 2", len(got))
	}
	if got[0] != idCall1 {
		t.Errorf("call 1 handler ctx correlation_id = %q, want this POST's %q (not the session-init %q)", got[0], idCall1, idInit)
	}
	if got[1] != idCall2 {
		t.Errorf("call 2 handler ctx correlation_id = %q, want this POST's %q (not the session-init %q)", got[1], idCall2, idInit)
	}
	if got[0] == got[1] {
		t.Errorf("handler ctx correlation_id was %q on both calls; each tools/call POST sent a different X-Correlation-ID so the values must differ", got[0])
	}
}

// TestHandlerContext_SubjectTokenFollowsCurrentRequest pins that
// RawSubjectTokenFromContext on the tool handler ctx is the bearer presented
// on THIS tools/call, not the token that opened the session.
//
// Two CallTools on one session present distinct bearers with the same UserID
// (so the SDK does not 403). The HTTP layer must observe each POST's bearer
// (harness check). The handler ctx must match that POST, so the two handler
// observations are different.
func TestHandlerContext_SubjectTokenFollowsCurrentRequest(t *testing.T) {
	t.Parallel()

	const (
		tokenInit  = "subject-token-session-init"
		tokenCall1 = "subject-token-call-1"
		tokenCall2 = "subject-token-call-2"
		userID     = "same-user"
	)
	accepted := map[string]struct{}{
		tokenInit:  {},
		tokenCall1: {},
		tokenCall2: {},
	}
	verifier := func(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if _, ok := accepted[token]; !ok {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{
			UserID:     userID,
			Expiration: time.Now().Add(time.Hour),
		}, nil
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	h := &metaStubHandler{name: "token-freshness-tool"}
	h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
		tok, _ := auth.RawSubjectTokenFromContext(ctx)
		mu.Lock()
		seen = append(seen, tok)
		mu.Unlock()
		return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}

	server := registerFreshnessTool(t, h)

	inbound := &inboundRecorder{key: "Authorization"}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var handler http.Handler = sdkauth.RequireBearerToken(verifier, nil)(streamable)
	handler = inbound.wrap(handler)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	tripper := &headerTripper{}
	httpClient := ts.Client()
	tripper.base = httpClient.Transport
	httpClient.Transport = tripper

	tripper.set("Authorization", "Bearer "+tokenInit)
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	call := func(tok string) {
		t.Helper()
		tripper.set("Authorization", "Bearer "+tok)
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      h.name,
			Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		want := "Bearer " + tok
		if got := inbound.lastValue(); got != want {
			t.Fatalf("HTTP POST carried Authorization=%q, want %q (client header did not reach the server)", got, want)
		}
	}

	call(tokenCall1)
	call(tokenCall2)

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("handler ran %d time(s), want 2", len(got))
	}
	if got[0] != tokenCall1 {
		t.Errorf("call 1 handler ctx subject token = %q, want this POST's %q (not the session-init token)", got[0], tokenCall1)
	}
	if got[1] != tokenCall2 {
		t.Errorf("call 2 handler ctx subject token = %q, want this POST's %q (not the session-init token)", got[1], tokenCall2)
	}
	if got[0] == got[1] {
		t.Errorf("handler ctx subject token was %q on both calls; each tools/call POST sent a different bearer so the values must differ", got[0])
	}
}

type handlerObservation struct {
	token string
	corr  string
}

type inboundLog struct {
	mu   sync.Mutex
	seen []handlerObservation
}

func (l *inboundLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			l.mu.Lock()
			l.seen = append(l.seen, handlerObservation{
				token: req.Header.Get("Authorization"),
				corr:  req.Header.Get(correlation.HeaderCorrelationID),
			})
			l.mu.Unlock()
		}
		next.ServeHTTP(w, req)
	})
}

func (l *inboundLog) snapshot() []handlerObservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]handlerObservation, len(l.seen))
	copy(out, l.seen)
	return out
}

// TestHandlerContext_ConcurrentCallsOnSameSessionAreIsolated pins that two
// in-flight tools/call POSTs on one streamable-HTTP session do not share
// handler-ctx request data. Each POST carries its own bearer and
// X-Correlation-ID; the handler ctx for that call must match that POST,
// not the session-init values and not the sibling call.
func TestHandlerContext_ConcurrentCallsOnSameSessionAreIsolated(t *testing.T) {
	t.Parallel()

	const (
		tokenInit = "subject-token-session-init"
		tokenA    = "subject-token-concurrent-a"
		tokenB    = "subject-token-concurrent-b"
		corrInit  = "corr-session-init"
		corrA     = "corr-concurrent-a"
		corrB     = "corr-concurrent-b"
		userID    = "same-user"
	)
	accepted := map[string]struct{}{
		tokenInit: {},
		tokenA:    {},
		tokenB:    {},
	}
	verifier := func(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if _, ok := accepted[token]; !ok {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{
			UserID:     userID,
			Expiration: time.Now().Add(time.Hour),
		}, nil
	}

	var entered sync.WaitGroup
	entered.Add(2)
	released := make(chan struct{})
	go func() {
		entered.Wait()
		close(released)
	}()

	var (
		mu   sync.Mutex
		seen []handlerObservation
	)
	h := &metaStubHandler{name: "concurrent-isolation-tool"}
	h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
		entered.Done()
		select {
		case <-released:
		case <-time.After(2 * time.Second):
		}
		tok, _ := auth.RawSubjectTokenFromContext(ctx)
		mu.Lock()
		seen = append(seen, handlerObservation{token: tok, corr: correlation.From(ctx)})
		mu.Unlock()
		return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}

	server := registerFreshnessTool(t, h)

	inbound := &inboundLog{}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var handler http.Handler = sdkauth.RequireBearerToken(verifier, nil)(streamable)
	handler = correlation.Middleware(handler)
	handler = inbound.wrap(handler)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	tripper := &headerTripper{}
	httpClient := ts.Client()
	tripper.base = httpClient.Transport
	httpClient.Transport = tripper

	tripper.set("Authorization", "Bearer "+tokenInit)
	tripper.set(correlation.HeaderCorrelationID, corrInit)
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	type call struct {
		token string
		corr  string
	}
	calls := []call{
		{token: tokenA, corr: corrA},
		{token: tokenB, corr: corrB},
	}

	var callWg sync.WaitGroup
	errCh := make(chan error, len(calls))
	for _, c := range calls {
		callWg.Add(1)
		go func(c call) {
			defer callWg.Done()
			hdr := make(http.Header)
			hdr.Set("Authorization", "Bearer "+c.token)
			hdr.Set(correlation.HeaderCorrelationID, c.corr)
			_, err := session.CallTool(withTripperHeaders(context.Background(), hdr), &mcp.CallToolParams{
				Name:      h.name,
				Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
			})
			if err != nil {
				errCh <- err
			}
		}(c)
	}
	callWg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("CallTool: %v", err)
	}

	gotInbound := inbound.snapshot()
	var sawA, sawB bool
	for _, in := range gotInbound {
		switch {
		case in.token == "Bearer "+tokenA && in.corr == corrA:
			sawA = true
		case in.token == "Bearer "+tokenB && in.corr == corrB:
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("HTTP POSTs did not carry both distinct (Authorization, X-Correlation-ID) pairs; inbound=%v", gotInbound)
	}

	mu.Lock()
	got := append([]handlerObservation(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("handler ran %d time(s), want 2", len(got))
	}

	want := map[handlerObservation]struct{}{
		{token: tokenA, corr: corrA}: {},
		{token: tokenB, corr: corrB}: {},
	}
	for _, obs := range got {
		if _, ok := want[obs]; !ok {
			t.Errorf("handler ctx saw token=%q correlation_id=%q; want each in-flight POST's own pair ({%q, %q} and {%q, %q}), not the session-init {%q, %q} and not the sibling call",
				obs.token, obs.corr, tokenA, corrA, tokenB, corrB, tokenInit, corrInit)
		}
	}
}

// TestHandlerContext_GeneratedCorrelationIDFollowsCurrentRequest pins that
// when the client omits both correlation headers, HTTP middleware generates
// a per-POST ID, stamps it inbound (so Extra sees it), and the handler ctx
// matches THIS POST's generated ID, not initialize's.
func TestHandlerContext_GeneratedCorrelationIDFollowsCurrentRequest(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		seen []string
	)
	h := &metaStubHandler{name: "corr-generated-freshness-tool"}
	h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
		mu.Lock()
		seen = append(seen, correlation.From(ctx))
		mu.Unlock()
		return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}

	server := registerFreshnessTool(t, h)
	stamped := &inboundRecorder{key: correlation.HeaderCorrelationID}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler := correlation.Middleware(stamped.wrap(streamable))

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           ts.Client(),
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	initID := stamped.lastValue()
	if initID == "" {
		t.Fatal("initialize POST had no stamped X-Correlation-ID")
	}

	_, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      h.name,
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	callID := stamped.lastValue()
	if callID == "" {
		t.Fatal("tools/call POST had no stamped X-Correlation-ID")
	}
	if callID == initID {
		t.Fatalf("tools/call reused initialize's generated ID %q; each POST must mint its own", initID)
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("handler ran %d time(s), want 1", len(got))
	}
	if got[0] != callID {
		t.Errorf("handler ctx correlation_id = %q, want this POST's generated %q (not initialize %q)", got[0], callID, initID)
	}
}
