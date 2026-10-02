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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestServerAddToolHasOneCallSite enforces SOL-153693's AC 4: the SDK's
// mcp.Server.AddTool must be reachable from exactly one place in this
// package, so a future tool that does not fit the standard shape cannot
// bypass ToolManager's input validation by adding a second, unpoliced call
// site — which is exactly how list-brokers and describe-semp-schema ended up
// with no input validation at all before this ticket. Metadata.NoBroker
// (types.go) is the supported way to add a no-broker tool now; see
// ToolHandler.Metadata, ToolManager.Register, and register.go's
// RegisterWithServer.
//
// A grep for ".AddTool(" would almost work, but would also match a mention
// inside a comment or string literal; this walks the AST instead, the same
// approach audit_error_type_drift_test.go uses for an analogous invariant,
// so a false positive can't make the guard flaky and a false negative can't
// make it silently toothless.
func TestServerAddToolHasOneCallSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fset := token.NewFileSet()
	type site struct {
		file string
		line int
	}
	var sites []site

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AddTool" {
				return true
			}
			pos := fset.Position(sel.Pos())
			sites = append(sites, site{file: name, line: pos.Line})
			return true
		})
	}

	if len(sites) == 0 {
		t.Fatal("found no AddTool call site at all; the scanner has stopped matching the " +
			"code it is meant to guard, so this test is no longer protecting anything")
	}

	if len(sites) > 1 {
		where := make([]string, 0, len(sites))
		for _, s := range sites {
			where = append(where, fmt.Sprintf("%s:%d", s.file, s.line))
		}
		t.Errorf("server.AddTool is called from %d places, want exactly 1: %s.\n"+
			"Every tool must reach the SDK through RegisterWithServer, which wraps "+
			"every registration with ToolManager's input validation. A second call "+
			"site is how list-brokers and describe-semp-schema ended up unvalidated "+
			"before SOL-153693 — they took no broker parameter and so could not use "+
			"the standard path, and bypassing ToolManager looked like the only way "+
			"to register them. It no longer is: give a tool that does not fit the "+
			"standard shape Metadata.NoBroker: true and register it into the "+
			"ToolManager, rather than calling server.AddTool directly.",
			len(sites), strings.Join(where, ", "))
	}

	if sites[0].file != "register.go" {
		t.Errorf("the one server.AddTool call site is in %s, want register.go "+
			"(RegisterWithServer) — update this test if the call site moved deliberately",
			sites[0].file)
	}
}
