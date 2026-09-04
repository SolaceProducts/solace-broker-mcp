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
	"fmt"
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

// emitFunc is the funnel every error_type passes through. Checking its call
// sites is what stops a new emit site from escaping this guard simply by
// naming its local something other than errorType.
const emitFunc = "logToolResult"

// emitErrorTypeArg is the position of the error_type argument in a
// logToolResult call: (ctx, tool, broker, start, errorType, toolErr, id).
const emitErrorTypeArg = 4

// checkEmitCallSite requires every logToolResult call to pass &errorType.
//
// The scan below is keyed on the variable NAME, which is a real limitation: a
// new emit site whose local is called anything else contributes nothing and
// nothing else would notice. Rather than pretend to do full dataflow analysis,
// this fails loudly on the shape it cannot follow, and names the fix.
func checkEmitCallSite(t *testing.T, fset *token.FileSet, file string, call *ast.CallExpr) {
	t.Helper()
	if len(call.Args) <= emitErrorTypeArg {
		t.Errorf("%s: %s is called with %d arguments; this guard reads the error_type at index %d. "+
			"If the signature changed, update emitErrorTypeArg.", position(fset, call), emitFunc, len(call.Args), emitErrorTypeArg)
		return
	}
	arg := call.Args[emitErrorTypeArg]
	unary, ok := arg.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		t.Errorf("%s: %s receives an error_type argument this guard cannot follow (%T). "+
			"Pass &%s, or extend the scanner.", position(fset, arg), emitFunc, arg, errorTypeVar)
		return
	}
	ident, ok := unary.X.(*ast.Ident)
	if !ok || ident.Name != errorTypeVar {
		t.Errorf("%s: %s receives &%s rather than &%s. This guard finds values by scanning "+
			"assignments to a variable of that name, so this emit site's error_type values are "+
			"never checked against audit.ErrorTypes(). Rename the local, or extend the scanner.",
			position(fset, arg), emitFunc, exprName(unary.X), errorTypeVar)
	}
}

// exprName renders an expression for a failure message.
func exprName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return fmt.Sprintf("%T", expr)
}

// collectDeclaredErrorTypes handles `var errorType = "x"`, which is a DeclStmt
// rather than an AssignStmt and was invisible to the first version of this
// scanner.
func collectDeclaredErrorTypes(t *testing.T, fset *token.FileSet, file string, decl *ast.DeclStmt,
	found map[string]string, byFile map[string]int, imports map[string]string, pkgCache map[string]map[string]string) {
	t.Helper()
	gen, ok := decl.Decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.VAR {
		return
	}
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if name.Name != errorTypeVar || i >= len(vs.Values) {
				continue
			}
			value, ok := stringConstant(t, vs.Values[i], imports, pkgCache)
			if !ok {
				t.Errorf("%s: errorType is declared from an expression the drift guard cannot "+
					"read (%T)", position(fset, vs.Values[i]), vs.Values[i])
				continue
			}
			if record(fset, vs.Values[i], value, found) {
				byFile[file]++
			}
		}
	}
}

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
// The scanner FAILS THE TEST rather than skipping, on three shapes it cannot
// follow: an assignment whose value it cannot read, a call to a function that
// does not exist in this package, and a logToolResult call that passes
// anything other than &errorType. Skipping was the original hole — an
// error_type introduced through a named constant was invisible to a
// literal-only scan, so a thirteenth value could ship, be silently rejected by
// the constructor at runtime, and leave this test green.
//
// What it still cannot do, stated plainly: it finds values by NAME, scanning
// assignments to a variable called errorType. It does no dataflow analysis. A
// value that reaches the emit funnel by some route none of the three checks
// above can see would go unchecked. The consequence is bounded rather than
// silent — the constructor rejects the unknown value and an audit_drop record
// tells the operator a destructive call went unrecorded (see
// TestAuditRecord_constructorRejectionEmitsADrop) — but this test is the first
// line, not the only one.
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

	// Pass 0: every logToolResult call site must pass the variable this
	// scanner tracks. That call is the funnel every error_type goes through,
	// so a new emit site whose local is named something else — which the
	// name-keyed scan below would not see at all — is caught here instead of
	// going silently unchecked.
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != emitFunc {
				return true
			}
			checkEmitCallSite(t, fset, name, call)
			return true
		})
	}

	// imports resolves a package qualifier (e.g. "metrics" in
	// metrics.ErrorTypePanic) to its import path, so stringConstant can follow
	// a cross-package named constant — the shape every error_type value took
	// on once SOL-152091 introduced the typed metrics.ErrorType vocabulary.
	// pkgCache memoizes each resolved package's own constants so a directory
	// is parsed once no matter how many selector expressions reference it.
	imports := buildImportMap(files)
	pkgCache := make(map[string]map[string]string)

	// Pass 1: every assignment to errorType. Records literal and named-constant
	// values directly, and remembers which functions feed the variable.
	found := make(map[string]string)
	byFile := make(map[string]int)
	classifiers := make(map[string]struct{})

	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				collectAssignedErrorTypes(t, fset, name, node, found, byFile, classifiers, imports, pkgCache)
			case *ast.DeclStmt:
				// `var errorType = "x"` is a DeclStmt, not an AssignStmt, and
				// was invisible to the earlier scanner.
				collectDeclaredErrorTypes(t, fset, name, node, found, byFile, imports, pkgCache)
			}
			return true
		})
	}

	// Every function named as feeding errorType must actually exist in this
	// package, or its returns are never scanned and its values go unchecked.
	declared := make(map[string]struct{})
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				declared[fn.Name.Name] = struct{}{}
			}
			return true
		})
	}
	for name := range classifiers {
		if _, ok := declared[name]; !ok {
			t.Errorf("errorType is assigned from %s(...), which is not declared in this package, "+
				"so its error_type values are never scanned. Resolve it, or assign from a value "+
				"this guard can read.", name)
		}
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
			collectClassifierErrorTypes(t, fset, name, fn, found, byFile, imports, pkgCache)
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
	found map[string]string, byFile map[string]int, classifiers map[string]struct{},
	imports map[string]string, pkgCache map[string]map[string]string) {
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
		value, ok := stringConstant(t, rhs, imports, pkgCache)
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
	found map[string]string, byFile map[string]int, imports map[string]string, pkgCache map[string]map[string]string) {
	t.Helper()
	ast.Inspect(fn, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		value, ok := stringConstant(t, ret.Results[0], imports, pkgCache)
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
// written inline, named in this package, or named in another package of this
// module reached through a selector (metrics.ErrorTypePanic).
//
// The local-Ident case is the one the original scanner missed: `errorType =
// errTypeTimeout` where errTypeTimeout is a package constant looked exactly
// like an unreadable expression and was silently skipped. The SelectorExpr
// case is the one SOL-152091 opened: once error_type values became typed
// metrics.ErrorType constants instead of bare string literals, every emit
// site in this package took that shape, and without this case the scanner
// would fail on all of them, not just a new one.
func stringConstant(t *testing.T, expr ast.Expr, imports map[string]string, pkgCache map[string]map[string]string) (string, bool) {
	t.Helper()
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
			return stringConstant(t, spec.Values[i], imports, pkgCache)
		}
	case *ast.SelectorExpr:
		pkgIdent, ok := node.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		importPath, ok := imports[pkgIdent.Name]
		if !ok {
			return "", false
		}
		dir, ok := resolveModuleDir(importPath)
		if !ok {
			return "", false
		}
		consts, cached := pkgCache[dir]
		if !cached {
			consts = packageConstants(t, dir)
			pkgCache[dir] = consts
		}
		value, ok := consts[node.Sel.Name]
		return value, ok
	}
	return "", false
}

// buildImportMap resolves every package qualifier used across the scanned
// files (e.g. "metrics") to its import path, merging all files' imports into
// one map. A single merged map is safe here — this is a small, hand-written
// guard test, not a general-purpose tool, and nothing in this package aliases
// two different import paths to the same name.
func buildImportMap(files map[string]*ast.File) map[string]string {
	imports := make(map[string]string)
	for _, file := range files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = path
		}
	}
	return imports
}

// modulePath is this module's own path, the only prefix resolveModuleDir
// knows how to turn into a directory on disk — a third-party import selector
// (there are none in this package's error_type assignments today) falls
// through as unreadable rather than guessing at $GOPATH or the module cache.
const modulePath = "github.com/SolaceProducts/solace-broker-mcp/"

// resolveModuleDir maps an in-module import path to the directory it lives in
// on disk, relative to this test's own working directory (internal/tools).
func resolveModuleDir(importPath string) (string, bool) {
	if !strings.HasPrefix(importPath, modulePath) {
		return "", false
	}
	return filepath.Join("../..", importPath[len(modulePath):]), true
}

// packageConstants parses every non-test .go file in dir and returns a map of
// top-level const name to its string value, for consts declared directly from
// a string literal — typed (`ErrorTypePanic ErrorType = "panic"`) or not.
// Fails the test rather than returning an empty map on a read or parse error:
// this scanner has no legitimate reason to be pointed at a directory it
// cannot read, so a failure here means resolveModuleDir or an import path is
// wrong, not that the target package has no constants.
func packageConstants(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s (resolved for a cross-package error_type constant): %v", dir, err)
	}
	fset := token.NewFileSet()
	consts := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, constName := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok {
						continue
					}
					if value, ok := unquoteString(lit); ok {
						consts[constName.Name] = value
					}
				}
			}
		}
	}
	return consts
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
