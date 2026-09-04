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

// Drift guard between the error_type values this package computes and the
// closed vocabulary the audit constructor accepts (SOL-152090).
//
// Why this exists. The error_type vocabulary was documented as ten values, and
// a later revision corrected it to eleven. Both were wrong: this package emits
// TWELVE, because two of the four logToolResult call sites are standalone
// tools that bypass ToolManager.CallTool and contribute bad_request and
// not_found, and every count so far was taken by reading manager.go alone.
//
// A prose count cannot hold this. So instead of counting, this test reads the
// AST of every non-test file in the package, collects every string literal
// that becomes an error_type, and compares the set against audit.ErrorTypes().
// Adding a thirteenth value in this package without adding it to the audit
// vocabulary fails here — at the point of the change — rather than shipping a
// record the audit constructor silently rejects at runtime.

package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
)

// classifierFunc is the one function that returns an error_type as a result
// rather than assigning it to the errorType variable. Its string literals are
// collected from its return statements.
const classifierFunc = "classifyBrokerError"

// errorTypeVar is the local variable every other emit site assigns to before
// handing it to logToolResult.
const errorTypeVar = "errorType"

// TestErrorTypeVocabularyMatchesAuditConstructor is the drift guard.
func TestErrorTypeVocabularyMatchesAuditConstructor(t *testing.T) {
	t.Parallel()

	found := scanErrorTypeLiterals(t)
	if len(found) == 0 {
		t.Fatal("scanned no error_type literals; the scanner has stopped matching the code " +
			"it is meant to guard, so this test is no longer protecting anything")
	}

	inCode := make([]string, 0, len(found))
	for value := range found {
		inCode = append(inCode, value)
	}
	sort.Strings(inCode)

	inAudit := make(map[string]struct{}, len(audit.ErrorTypes()))
	for _, v := range audit.ErrorTypes() {
		inAudit[v] = struct{}{}
	}

	for _, value := range inCode {
		if _, ok := inAudit[value]; !ok {
			t.Errorf("error_type %q is computed at %s but is not in audit.ErrorTypes().\n"+
				"Add it to errorTypeVocabulary in internal/observability/audit/event.go, "+
				"and to the error_type table in docs/observability.md, in this same change.",
				value, found[value])
		}
	}
	for _, value := range audit.ErrorTypes() {
		if _, ok := found[value]; !ok {
			t.Errorf("audit.ErrorTypes() accepts %q but nothing in internal/tools computes it. "+
				"Either a call site was removed and the vocabulary was not, or the scanner "+
				"below no longer sees the site that produces it.", value)
		}
	}

	// The count is derived from the scan, never written down, so it cannot go
	// stale the way the ticket's own count did. Logged so a reviewer reading
	// CI output sees the number the code actually supports.
	t.Logf("error_type vocabulary: %d values computed in internal/tools, %d accepted by audit: %v",
		len(inCode), len(audit.ErrorTypes()), inCode)
}

// scanErrorTypeLiterals returns every error_type string literal in this
// package's non-test files, mapped to the file:line that produced it.
//
// Two shapes are collected, matching the only two the package uses:
//
//	errorType = "x"  /  errorType := "x"  — assignment to the local variable
//	return "x", ...                       — classifyBrokerError's results
//
// Scanning the whole package directory rather than a fixed file list is
// deliberate: a new emit site in a new file is exactly the drift this guards.
func scanErrorTypeLiterals(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fset := token.NewFileSet()
	found := make(map[string]string)

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
			switch node := n.(type) {
			case *ast.AssignStmt:
				collectAssignedErrorTypes(fset, node, found)
			case *ast.FuncDecl:
				if node.Name.Name == classifierFunc {
					collectClassifierErrorTypes(fset, node, found)
				}
			}
			return true
		})
	}
	return found
}

// collectAssignedErrorTypes records the literals in `errorType = "x"` and
// `errorType := "x"`, position by position so a multi-value assignment only
// contributes the value that actually lands in errorType.
func collectAssignedErrorTypes(fset *token.FileSet, stmt *ast.AssignStmt, found map[string]string) {
	for i, lhs := range stmt.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name != errorTypeVar || i >= len(stmt.Rhs) {
			continue
		}
		if value, ok := stringLiteral(stmt.Rhs[i]); ok {
			record(fset, stmt.Rhs[i], value, found)
		}
	}
}

// collectClassifierErrorTypes records the first result of every return in
// classifyBrokerError, which is an error_type by that function's contract.
func collectClassifierErrorTypes(fset *token.FileSet, fn *ast.FuncDecl, found map[string]string) {
	ast.Inspect(fn, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		if value, ok := stringLiteral(ret.Results[0]); ok {
			record(fset, ret.Results[0], value, found)
		}
		return true
	})
}

// stringLiteral unwraps an expression that is a plain string constant.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// record keeps the first position seen for a value, so the failure message
// points at a real line.
func record(fset *token.FileSet, expr ast.Expr, value string, found map[string]string) {
	if _, seen := found[value]; seen {
		return
	}
	pos := fset.Position(expr.Pos())
	found[value] = filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
}
