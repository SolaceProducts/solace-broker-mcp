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

// errorTypeVar is the local variable every emit site assigns to before handing
// it to logToolResult.
const errorTypeVar = "errorType"

// contributingFiles are the files known to compute an error_type. Each must
// still contribute at least one value.
//
// Without this check, a file going dark to the scanner is invisible whenever
// every value it produces is also produced elsewhere: register.go's three
// values (bad_request, panic, marshal_error) are all duplicated in the other
// two files, so losing sight of it entirely would leave the set unchanged and
// the test green. Listing the files is not the guard against a NEW file —
// scanErrorTypeLiterals reads the whole package directory for that — it is the
// guard against silently losing an OLD one.
var contributingFiles = []string{
	"manager.go",
	"register.go",
	"describe_semp_schema.go",
}

// TestErrorTypeVocabularyMatchesAuditConstructor is the drift guard.
func TestErrorTypeVocabularyMatchesAuditConstructor(t *testing.T) {
	t.Parallel()

	found, byFile := scanErrorTypeLiterals(t)
	if len(found) == 0 {
		t.Fatal("scanned no error_type literals; the scanner has stopped matching the code " +
			"it is meant to guard, so this test is no longer protecting anything")
	}
	for _, name := range contributingFiles {
		if byFile[name] == 0 {
			t.Errorf("%s contributed no error_type values. Either its emit sites were removed, "+
				"or the scanner below stopped recognising their shape — and because that file's "+
				"values are also produced elsewhere, nothing else in this test would notice.", name)
		}
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

// scanErrorTypeLiterals returns every error_type value this package's non-test
// files compute, mapped to the file:line that produced it, plus a per-file
// count.
//
// It collects assignments to the errorType variable, and — because one site
// assigns from a call rather than a literal — the results of every in-package
// function whose return value lands in errorType. The set of such functions is
// discovered from the assignments themselves rather than hardcoded, so adding
// a second classifier helper is covered automatically.
//
// The scanner FAILS THE TEST on any assignment whose value it cannot read,
// rather than skipping it. Skipping was the original hole: an error_type
// introduced through a named constant is invisible to a literal-only scan, so
// a thirteenth value could ship and be silently rejected by the constructor at
// runtime while this test stayed green. Anything unreadable is now a loud
// failure telling the author to extend the scanner.
//
// Scanning the whole package directory rather than a fixed file list is
// deliberate: a new emit site in a new file is exactly the drift this guards.
func scanErrorTypeLiterals(t *testing.T) (map[string]string, map[string]int) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Mode 0, so object resolution stays ON: it is what lets an *ast.Ident
		// on the right-hand side be traced back to the constant it names.
		// Passing parser.SkipObjectResolution here would silently reopen the
		// named-constant hole this scanner was rewritten to close.
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = file
	}

	// Pass 1: every assignment to errorType. Records literal and named-constant
	// values directly, and remembers which functions feed the variable.
	found := make(map[string]string)
	byFile := make(map[string]int)
	classifiers := make(map[string]struct{})

	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			collectAssignedErrorTypes(t, fset, name, stmt, found, byFile, classifiers)
			return true
		})
	}

	// Pass 2: the first result of every return in each function discovered
	// above. classifyBrokerError's contract is that its first result IS an
	// error_type, and any future sibling helper's must be too.
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if _, feeds := classifiers[fn.Name.Name]; !feeds {
				return true
			}
			collectClassifierErrorTypes(t, fset, name, fn, found, byFile)
			return true
		})
	}

	return found, byFile
}

// collectAssignedErrorTypes records what `errorType = X` / `errorType := X`
// assigns, position by position so a multi-value assignment only contributes
// the value that actually lands in errorType.
//
// A call on the right-hand side is not a value; it names a function whose
// results are scanned in pass 2. Anything else is unreadable and fails.
func collectAssignedErrorTypes(t *testing.T, fset *token.FileSet, file string, stmt *ast.AssignStmt,
	found map[string]string, byFile map[string]int, classifiers map[string]struct{}) {
	t.Helper()
	for i, lhs := range stmt.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name != errorTypeVar {
			continue
		}
		// A multi-value assignment from one call (errorType, toolErr = f(...))
		// has a single Rhs entry; a positional one has as many as the Lhs.
		rhs := stmt.Rhs[0]
		if len(stmt.Rhs) == len(stmt.Lhs) {
			rhs = stmt.Rhs[i]
		}

		if call, isCall := rhs.(*ast.CallExpr); isCall {
			if name, ok := calleeName(call); ok {
				classifiers[name] = struct{}{}
				continue
			}
			t.Errorf("%s: errorType is assigned from a call the drift guard cannot name. "+
				"Extend calleeName, or this emit site's values go unchecked.", position(fset, rhs))
			continue
		}
		value, ok := stringConstant(rhs)
		if !ok {
			t.Errorf("%s: errorType is assigned from an expression the drift guard cannot read "+
				"(%T). Extend stringConstant, or this value can ship without being added to "+
				"audit.ErrorTypes() and will be silently rejected at runtime.", position(fset, rhs), rhs)
			continue
		}
		if record(fset, rhs, value, found) {
			byFile[file]++
		}
	}
}

// collectClassifierErrorTypes records the first result of every return in a
// function whose return value feeds errorType.
func collectClassifierErrorTypes(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl,
	found map[string]string, byFile map[string]int) {
	t.Helper()
	ast.Inspect(fn, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		value, ok := stringConstant(ret.Results[0])
		if !ok {
			t.Errorf("%s: %s returns an error_type the drift guard cannot read (%T)",
				position(fset, ret.Results[0]), fn.Name.Name, ret.Results[0])
			return true
		}
		if record(fset, ret.Results[0], value, found) {
			byFile[file]++
		}
		return true
	})
}

// calleeName returns the name of a called function, for a plain call or a
// method call on a receiver (m.classifyBrokerError(...)).
func calleeName(call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, true
	case *ast.SelectorExpr:
		return fn.Sel.Name, true
	}
	return "", false
}

// stringConstant reads an expression that is a string constant, whether
// written inline or named.
//
// The named case is the one the original scanner missed: `errorType =
// errTypeTimeout` where errTypeTimeout is a package constant looked exactly
// like an unreadable expression and was silently skipped.
func stringConstant(expr ast.Expr) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		return unquoteString(node)
	case *ast.Ident:
		// Resolve the identifier to its declaration and read the constant.
		if node.Obj == nil {
			return "", false
		}
		spec, ok := node.Obj.Decl.(*ast.ValueSpec)
		if !ok {
			return "", false
		}
		for i, name := range spec.Names {
			if name.Name != node.Name || i >= len(spec.Values) {
				continue
			}
			return stringConstant(spec.Values[i])
		}
	}
	return "", false
}

// unquoteString unwraps a string BasicLit.
func unquoteString(lit *ast.BasicLit) (string, bool) {
	if lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// position renders file:line for a failure message.
func position(fset *token.FileSet, expr ast.Expr) string {
	pos := fset.Position(expr.Pos())
	return filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
}

// record keeps the first position seen for a value, so the failure message
// points at a real line. Reports whether this site was counted, which is
// always: the per-file tally counts emit SITES, not distinct values, so a file
// producing only values another file also produces still registers.
func record(fset *token.FileSet, expr ast.Expr, value string, found map[string]string) bool {
	if _, seen := found[value]; !seen {
		found[value] = position(fset, expr)
	}
	return true
}
