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
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests pin the cacheScope the server puts on tools/list (SOL-156384).
// The hook decides a wire field, so the main test goes through a real client and
// handler and reads the field back, rather than calling the hook and asserting
// on what it was told to write.

// TestToolsListCacheable_OnlyToolsList pins the hook's reach. The SDK calls it
// for every result that carries cache fields; only tools/list is ours to change.
func TestToolsListCacheable_OnlyToolsList(t *testing.T) {
	tests := []struct {
		name      string
		authz     bool
		req       mcp.Request
		wantScope string
	}{
		{"tools/list, authorization on", true, &mcp.ListToolsRequest{}, "private"},
		{"tools/list, authorization off", false, &mcp.ListToolsRequest{}, ""},
		{"prompts/list, authorization on", true, &mcp.ListPromptsRequest{}, ""},
		{"resources/list, authorization on", true, &mcp.ListResourcesRequest{}, ""},
		{"resources/templates/list, authorization on", true, &mcp.ListResourceTemplatesRequest{}, ""},
		{"resources/read, authorization on", true, &mcp.ReadResourceRequest{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &mcp.Cacheable{}
			toolsListCacheable(tt.authz)(context.Background(), tt.req, c)
			// "" is what the SDK then normalizes to "public".
			if c.CacheScope != tt.wantScope {
				t.Errorf("CacheScope = %q, want %q", c.CacheScope, tt.wantScope)
			}
			if c.TTLMs != 0 {
				t.Errorf("TTLMs = %d, want 0: the hook must leave ttlMs at the SDK default", c.TTLMs)
			}
		})
	}
}

// cacheScopeServer builds the production wiring over a real tool manager: the
// SDK options from newServerOptions, the registration pipeline, and
// installRequestMiddleware. Caller group "Ops" is granted get-vpn-status
// only, so list-queues is a tool the caller can neither see nor call.
//
// pageSize 1 forces tools/list onto several pages.
func cacheScopeServer(t *testing.T, cfg *config.ServerConfig, pageSize int) *mcp.Server {
	t.Helper()

	opts := newServerOptions(cfg)
	opts.PageSize = pageSize
	server := mcp.NewServer(&mcp.Implementation{Name: "cache-scope-test", Version: "0"}, opts)

	pool := semp.NewBrokerPool(budgetTestConfig(t), nil)
	t.Cleanup(pool.Close)
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	compositeTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, composite.NewCompositeExecutor(operations))
	tools.RegisterListBrokers(mgr, pool)

	policy := policyFrom(t, cfg)
	tools.RegisterWithServer(mgr, server, pool, false, policy, "groups")
	installRequestMiddleware(server, cfg, policy, "groups")
	return server
}

// scopeConfig is an oauth config with tool authorization on or off and the
// list filter on or off.
func scopeConfig(authzOn, filterOn bool) *config.ServerConfig {
	cfg := makeEnabledConfig()
	cfg.MCPClientAuth.ToolAuthorization.Enabled = &authzOn
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &filterOn
	cfg.MCPClientAuth.ToolAuthorization.AccessLevelGroups = map[string][]string{
		"Ops": {"get-vpn-status"},
	}
	return cfg
}

// bodyRecorder keeps every response body so a test can read the fields as they
// went over the wire, whatever the client did with them.
type bodyRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *bodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer cache-scope-token")
	resp, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, string(raw))
	r.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

func (r *bodyRecorder) wireContains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.bodies, func(b string) bool { return strings.Contains(b, sub) })
}

// connectAs serves server over HTTP behind a verifier that yields group "Ops"
// and returns a client session. stateless selects the transport, which in turn
// decides the protocol the SDK client negotiates: 2026-07-28 against a
// stateless handler, the legacy handshake (wireProtocolVersion) against a
// stateful one.
func connectAs(t *testing.T, server *mcp.Server, stateless bool) (*mcp.ClientSession, *bodyRecorder) {
	t.Helper()

	verifier := func(context.Context, string, *http.Request) (*sdkauth.TokenInfo, error) {
		return &sdkauth.TokenInfo{
			UserID:     "auth0|cache-scope",
			Expiration: time.Now().Add(time.Hour),
			Extra: map[string]any{
				"iss":                         "https://idp.cache-scope.example",
				"client_id":                   "cache-scope-client",
				"jti":                         "jti-cache-scope",
				authz.TokenInfoExtraKeyGroups: []string{"Ops"},
			},
		}, nil
	}
	handler := sdkauth.RequireBearerToken(verifier, nil)(
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			newMCPStreamableOptions(stateless)))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	rec := &bodyRecorder{}
	client := mcp.NewClient(&mcp.Implementation{Name: "cache-scope-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           &http.Client{Transport: rec},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, rec
}

// TestToolsListCacheScope_EveryPageOnTheWire is the acceptance test: over a real
// handler, on both protocol revisions, every page of tools/list carries the
// scope the configuration calls for.
func TestToolsListCacheScope_EveryPageOnTheWire(t *testing.T) {
	tests := []struct {
		name      string
		authz     bool
		filter    bool
		wantScope string
	}{
		{"authorization on, filter on", true, true, "private"},
		// Private whenever authorization is on, so the label does not move with
		// the filter flag.
		{"authorization on, filter off", true, false, "private"},
		{"authorization off", false, false, "public"},
	}
	transports := []struct {
		name        string
		stateless   bool
		wantVersion string
	}{
		{"2025-11-25 client (stateful transport)", false, wireProtocolVersion},
		{"2026-07-28 client (stateless transport)", true, sep2575ProtocolVersion},
	}
	for _, tt := range tests {
		for _, tr := range transports {
			t.Run(tt.name+"/"+tr.name, func(t *testing.T) {
				server := cacheScopeServer(t, scopeConfig(tt.authz, tt.filter), 1)
				session, rec := connectAs(t, server, tr.stateless)

				if got := session.InitializeResult().ProtocolVersion; got != tr.wantVersion {
					t.Fatalf("negotiated %q, want %q; this case would not test the client it names", got, tr.wantVersion)
				}

				pages, cursor := 0, ""
				for {
					res, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: cursor})
					if err != nil {
						t.Fatalf("ListTools page %d: %v", pages+1, err)
					}
					pages++
					if res.CacheScope != tt.wantScope {
						t.Errorf("page %d cacheScope = %q, want %q", pages, res.CacheScope, tt.wantScope)
					}
					if res.TTLMs != 0 {
						t.Errorf("page %d ttlMs = %d, want 0", pages, res.TTLMs)
					}
					if res.NextCursor == "" {
						break
					}
					cursor = res.NextCursor
				}
				if pages < 2 {
					t.Fatalf("got %d page(s); the per-page claim needs more than one", pages)
				}

				// The same fields as they crossed the wire, which is what a
				// gateway sees: present for this protocol revision, not merely
				// decoded by the client.
				if !rec.wireContains(`"cacheScope":"` + tt.wantScope + `"`) {
					t.Errorf("no response body carries cacheScope %q", tt.wantScope)
				}
				if !rec.wireContains(`"ttlMs":0`) {
					t.Error("no response body carries ttlMs 0")
				}
			})
		}
	}
}

// TestToolsListHiddenToolStillDeniedOnCall: the label is about visibility; the
// call-time check is what enforces access. A tool filtered out of the list must
// still be refused when called by name.
func TestToolsListHiddenToolStillDeniedOnCall(t *testing.T) {
	const hidden = "list-queues"
	session, _ := connectAs(t, cacheScopeServer(t, scopeConfig(true, true), 0), true)

	var listed []string
	cursor := ""
	for {
		res, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		for _, tool := range res.Tools {
			listed = append(listed, tool.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if slices.Contains(listed, hidden) {
		t.Fatalf("%q is in the filtered list %v; it must be hidden for this test to mean anything", hidden, listed)
	}
	if !slices.Contains(listed, "get-vpn-status") {
		t.Fatalf("granted tool get-vpn-status missing from %v; the filter removed too much", listed)
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      hidden,
		Arguments: map[string]any{"broker": "dev"},
	})
	if err != nil {
		t.Fatalf("CallTool %q: protocol error %v, want a tool-level denial", hidden, err)
	}
	if !res.IsError {
		t.Fatalf("hidden tool %q was not denied on tools/call: %+v", hidden, res)
	}
}
