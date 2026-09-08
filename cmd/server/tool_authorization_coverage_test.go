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
	"sort"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every tool the server exposes must be either policy-wrapped by
// RegisterWithServer or named by tools.IsExemptFromToolAuthorization. A tool that
// is neither vanishes from a filtered tools/list while staying callable by name —
// listed-implies-callable broken, silently.
//
// This must live in cmd/server: the property relates what main() registers to
// what the predicate enumerates, and internal/tools cannot see main(). A test
// there can only compare the predicate to a copy of itself, which is how
// describe-semp-schema stayed missing from the exemption.
//
// Superseded once exemption becomes a property of registration, so a new
// unpoliced AddTool cannot compile without declaring itself.

// gatedAndExposedTools rebuilds main()'s registration pipeline and returns the
// manager's tool names alongside what a client sees over the wire. One call for
// both, so the two sides cannot drift apart between them.
//
// enableWriteTools is true so the write gate hides nothing — this is about
// authorization coverage, not the default tool set.
func gatedAndExposedTools(t *testing.T) (gated []string, exposed []*mcp.Tool) {
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

	server := mcp.NewServer(&mcp.Implementation{Name: "solace-broker-mcp", Version: "test"}, nil)
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, composite.NewCompositeExecutor(operations))

	// This package's own unexported functions, so a native tool added later is
	// picked up here automatically.
	registerSEMPv1Tools(mgr)
	registerMixedTools(mgr)

	// Policy nil: whether a tool gets wrapped depends on where it was registered,
	// not on any grant, so a compiled policy would change nothing here.
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")
	tools.RegisterListBrokers(server, pool, nil)
	if err := tools.RegisterDescribeSempSchema(server, specs.FS, nil); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}

	for _, h := range mgr.Handlers() {
		gated = append(gated, h.Metadata().Name)
	}
	return gated, listAllTools(t, server)
}

// TestEveryRegisteredToolIsGatedOrExempt is the invariant: no tool escapes both
// the policy wrapper and the exemption list.
func TestEveryRegisteredToolIsGatedOrExempt(t *testing.T) {
	gatedNames, exposed := gatedAndExposedTools(t)

	gated := make(map[string]struct{}, len(gatedNames))
	for _, n := range gatedNames {
		gated[n] = struct{}{}
	}

	var unaccounted []string
	for _, tool := range exposed {
		_, isGated := gated[tool.Name]
		if isGated || tools.IsExemptFromToolAuthorization(tool.Name) {
			continue
		}
		unaccounted = append(unaccounted, tool.Name)
	}

	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		t.Errorf("tool(s) with neither a policy wrapper nor an exemption: %v — "+
			"callable by name but filtered out of tools/list. Register through the "+
			"tool manager, or add to tools.IsExemptFromToolAuthorization if "+
			"deliberately unpoliced.", unaccounted)
	}

	// A server exposing nothing would pass the loop above vacuously.
	if len(exposed) == 0 {
		t.Fatal("server exposed no tools; the registration pipeline did not run")
	}
}

// The other direction: an exemption naming a tool that no longer exists. Not a
// live bug — a stale exemption grants nothing — but it would absorb a rename
// silently, the old name keeping its exemption while the new one gets filtered.
func TestExemptToolsAreActuallyRegistered(t *testing.T) {
	_, exposed := gatedAndExposedTools(t)

	exposedNames := make(map[string]struct{}, len(exposed))
	for _, tool := range exposed {
		exposedNames[tool.Name] = struct{}{}
	}

	// Listed explicitly because the predicate is a function, not an iterable set.
	var exemptButAbsent []string
	for _, name := range []string{"list-brokers", "describe-semp-schema"} {
		if !tools.IsExemptFromToolAuthorization(name) {
			t.Errorf("%q expected exempt but the predicate disagrees; update this "+
				"test if the exemption was removed deliberately", name)
			continue
		}
		if _, ok := exposedNames[name]; !ok {
			exemptButAbsent = append(exemptButAbsent, name)
		}
	}

	if len(exemptButAbsent) > 0 {
		sort.Strings(exemptButAbsent)
		t.Errorf("exempt tool(s) not registered on the server: %v", exemptButAbsent)
	}
}
