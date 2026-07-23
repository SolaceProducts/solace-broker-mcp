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
	"os"
	"path/filepath"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// budgetTestConfig writes a minimal single-broker config to t.TempDir and
// loads it via the real config.LoadConfig validate+canonicalize path (same
// approach as internal/tools' writeTestConfig), rather than constructing
// *config.ServerConfig by hand — LoadConfig applies defaults/canonicalization
// semp.NewBrokerPool may depend on.
func budgetTestConfig(t *testing.T) *config.ServerConfig {
	t.Helper()
	body := "mcp_client_auth:\n  mode: disabled\nbrokers:\n" +
		"  dev:\n    url: http://localhost:8081\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n"
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// registeredServer builds the exact production tool-registration pipeline
// (mirrors main()'s steps 5-8: load composite tools, parse SEMP operations,
// build the executor and tool manager, register SEMPv1/mixed native tools,
// then RegisterWithServer + RegisterListBrokers) so this test measures what
// the server actually exposes, not a hand-maintained approximation of it.
// registerSEMPv1Tools/registerMixedTools are this package's own unexported
// functions — calling them directly guarantees this test can never drift
// from what main() registers, including any native tool added later.
func registeredServer(t *testing.T, enableWriteTools bool) *mcp.Server {
	t.Helper()
	cfg := budgetTestConfig(t)
	pool := semp.NewBrokerPool(cfg, nil)

	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	compositeTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	executor := composite.NewCompositeExecutor(operations)

	server := mcp.NewServer(&mcp.Implementation{Name: "solace-broker-mcp", Version: "test"}, nil)
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, executor)
	registerSEMPv1Tools(mgr)
	registerMixedTools(mgr)
	tools.RegisterWithServer(mgr, server, pool, enableWriteTools, nil, "")
	tools.RegisterListBrokers(server, pool)
	return server
}

// listAllTools drives a real tools/list round-trip over an in-memory MCP
// transport — the same client-visible surface a real agent sees, not an
// introspection shortcut — following pagination to completion so a future
// change that starts paginating tools/list doesn't silently under-measure.
func listAllTools(t *testing.T, server *mcp.Server) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "budget-test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	var all []*mcp.Tool
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all
}

// TestToolListBudget pins SOL-152125: the aggregate tools/list payload (every
// tool's name + description + input schema, summed) must stay within a sane
// LLM context budget. Thresholds carry ~3x headroom over the measured
// baseline at authoring time (19 read-only tools / ~21.1KB; 35 tools with
// writes enabled / ~40.6KB) specifically so routine description-wording edits
// don't trip this — it's a regression guard against unbounded growth, not a
// tight budget. A real regression (e.g. a tool description ballooning, or a
// new tool with an oversized schema) should still fail it well before the
// aggregate gets anywhere near actual LLM context limits.
//
// This threshold is a static ceiling, like a frontend bundle-size budget —
// expect to bump it deliberately as the tool catalog legitimately grows
// (roughly 400 tokens/tool at today's average, so there's headroom for
// several dozen more tools before that's needed). If this test fails,
// don't assume the tools are broken: check whether the growth is real and
// intentional (bump the threshold, in the same PR, with an updated comment)
// or an accident (a runaway description, a copy-pasted oversized schema) —
// the failure exists to force that judgment call, not to be silenced.
func TestToolListBudget(t *testing.T) {
	const bytesPerToken = 4 // same rough estimate used throughout this investigation

	cases := []struct {
		name             string
		enableWriteTools bool
		maxTokens        int
	}{
		{"read-only", false, 15_000},
		{"with-write-tools", true, 25_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := registeredServer(t, tc.enableWriteTools)
			allTools := listAllTools(t, server)

			payload, err := json.Marshal(allTools)
			if err != nil {
				t.Fatalf("marshal tools: %v", err)
			}
			tokens := len(payload) / bytesPerToken

			t.Logf("%s: %d tools, %d bytes, ~%d tokens (budget: %d tokens)",
				tc.name, len(allTools), len(payload), tokens, tc.maxTokens)

			if tokens > tc.maxTokens {
				t.Errorf("%s: aggregate tools/list payload is ~%d tokens, want <= %d — "+
					"a tool description or schema likely grew unexpectedly; see the top tools "+
					"by size before assuming the threshold itself needs raising",
					tc.name, tokens, tc.maxTokens)
			}
		})
	}
}
