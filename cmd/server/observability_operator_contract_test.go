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

// SOL-154894: operator-doc contracts beyond the metric-name scrape check.
// TestObservabilityDocMatchesRegistry already diffs mcp_* families against
// the Metrics tables. These tests pin the surfaces that check cannot see:
// OBS_* constants, observability YAML fields, /readyz probe names, mux
// routes, archived runbook headings, and additional code-backed scenarios
// found after the public runbook was compressed.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, elem ...string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, elem...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(elem...), err)
	}
	return string(raw)
}

func publicObservabilityDoc(t *testing.T) string {
	t.Helper()
	return readRepoFile(t, "docs", "observability.md")
}

func archivedObservabilityDoc(t *testing.T) string {
	t.Helper()
	return readRepoFile(t, "docs", "internal", "observability-schema-review.md")
}

func operatorRunbookSection(t *testing.T, raw string) string {
	t.Helper()
	const start = "## Operator Runbook"
	i := strings.Index(raw, start)
	if i < 0 {
		t.Fatalf("no %q heading", start)
	}
	section := raw[i:]
	if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(section[len(start):]); next != nil {
		section = section[:len(start)+next[0]]
	}
	return section
}

func normalizeHeading(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "/", " ")
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func h3Headings(section string) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "### ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "### ")))
		}
	}
	return out
}

func headingSet(headings []string) map[string]string {
	out := map[string]string{}
	for _, h := range headings {
		out[normalizeHeading(h)] = h
	}
	return out
}

var obsFlagConstRE = regexp.MustCompile(`envObs[A-Za-z]+(?:Retired)?\s*=\s*"(OBS_[A-Z0-9_]+)"`)

func TestObservabilityDoc_DocumentsEveryOBSFlag(t *testing.T) {
	src := readRepoFile(t, "internal", "config", "observability.go")
	flags := obsFlagConstRE.FindAllStringSubmatch(src, -1)
	if len(flags) == 0 {
		t.Fatal("no OBS_* constants found in internal/config/observability.go")
	}
	doc := publicObservabilityDoc(t)
	for _, m := range flags {
		name := m[1]
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("docs/observability.md does not mention `%s` — every OBS_* constant must appear so an operator can find the shipped switch", name)
		}
	}
}

var observabilityYAMLFieldRE = regexp.MustCompile("`yaml:\"([a-z0-9_]+)\"`")

func TestObservabilityDoc_DocumentsEveryYAMLField(t *testing.T) {
	src := readRepoFile(t, "internal", "config", "observability.go")
	typeStart := strings.Index(src, "type ObservabilityConfig struct {")
	if typeStart < 0 {
		t.Fatal("ObservabilityConfig struct not found")
	}
	body := src[typeStart:]
	end := strings.Index(body, "\n}")
	if end < 0 {
		t.Fatal("ObservabilityConfig struct close not found")
	}
	body = body[:end]
	doc := publicObservabilityDoc(t)
	found := 0
	for _, m := range observabilityYAMLFieldRE.FindAllStringSubmatch(body, -1) {
		field := m[1]
		found++
		if !strings.Contains(doc, "`observability."+field+"`") && !strings.Contains(doc, "`"+field+"`") {
			t.Errorf("docs/observability.md does not mention observability YAML field %q", field)
		}
	}
	if found == 0 {
		t.Fatal("no yaml tags found on ObservabilityConfig")
	}
}

func productionGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s := lit.Value
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`') {
		return s[1 : len(s)-1], true
	}
	return "", false
}

func TestObservabilityDoc_DocumentsEveryReadinessProbe(t *testing.T) {
	dir := "."
	files := productionGoFiles(t, dir)
	fset := token.NewFileSet()
	probes := map[string]bool{}
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RegisterListener" || len(call.Args) < 1 {
				return true
			}
			name, ok := stringLit(call.Args[0])
			if !ok {
				t.Errorf("%s: RegisterListener first argument is not a string literal — name it with a literal so the operator-doc contract can see it", path)
				return true
			}
			probes[name] = true
			return true
		})
	}
	if len(probes) == 0 {
		t.Fatal("no RegisterListener calls in cmd/server production files")
	}
	runbook := operatorRunbookSection(t, publicObservabilityDoc(t))
	for name := range probes {
		if !strings.Contains(runbook, "`"+name+"`") {
			t.Errorf("Operator Runbook does not mention readiness probe `%s` — a /readyz reason the process actually emits", name)
		}
	}
}

func TestObservabilityDoc_DocumentsMuxProbeRoutes(t *testing.T) {
	src := readRepoFile(t, "cmd", "server", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	routes := map[string]bool{}
	consts := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			if val, ok := stringLit(vs.Values[0]); ok {
				consts[vs.Names[0].Name] = val
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handle" || len(call.Args) < 1 {
			return true
		}
		path, ok := stringLit(call.Args[0])
		if !ok {
			if ident, ok := call.Args[0].(*ast.Ident); ok {
				path, ok = consts[ident.Name]
				if !ok {
					return true
				}
			} else {
				return true
			}
		}
		switch path {
		case "/livez", "/health", "/readyz", "/ready", "/metrics", "/mcp":
			routes[path] = true
		}
		return true
	})
	required := []string{"/livez", "/health", "/readyz", "/metrics", "/mcp"}
	doc := publicObservabilityDoc(t)
	for _, path := range required {
		if !routes[path] {
			t.Errorf("cmd/server/main.go no longer registers %s — update this test's required list", path)
			continue
		}
		if !strings.Contains(doc, "`"+path+"`") {
			t.Errorf("docs/observability.md does not mention route `%s`", path)
		}
	}
}

// archivedRunbookExclusions names archived ### headings that must not return
// to the public operator page, with the reason so a deletion cannot be silent.
var archivedRunbookExclusions = map[string]string{}

func TestObservabilityDoc_KeepsArchivedRunbookHeadings(t *testing.T) {
	archived := headingSet(h3Headings(operatorRunbookSection(t, archivedObservabilityDoc(t))))
	if len(archived) == 0 {
		t.Fatal("archived Operator Runbook has no ### headings")
	}
	public := headingSet(h3Headings(operatorRunbookSection(t, publicObservabilityDoc(t))))
	for key, original := range archived {
		if _, excluded := archivedRunbookExclusions[key]; excluded {
			if _, present := public[key]; present {
				t.Errorf("archived heading %q is listed as an exclusion but still present on the public page", original)
			}
			continue
		}
		if _, ok := public[key]; !ok {
			t.Errorf("archived runbook heading %q is missing from docs/observability.md Operator Runbook — restore it or add an evidence-backed exclusion", original)
		}
	}
}

// extraCodeBackedRunbookHeadings are operator-visible cases proven from
// production code after the archive was frozen. Each must exist as a ###
// heading on the public runbook.
var extraCodeBackedRunbookHeadings = []string{
	"Tracing provider failed to start",
	"A metrics family is missing after startup",
	"Broker answered with an application error",
	"OTLP push failed while scrape still works",
}

func TestObservabilityDoc_DocumentsExtraCodeBackedScenarios(t *testing.T) {
	public := headingSet(h3Headings(operatorRunbookSection(t, publicObservabilityDoc(t))))
	for _, heading := range extraCodeBackedRunbookHeadings {
		if _, ok := public[normalizeHeading(heading)]; !ok {
			t.Errorf("Operator Runbook is missing code-backed heading %q", heading)
		}
	}
}

var slogCallRE = regexp.MustCompile(`slog\.(Warn|Error)\(\s*"([^"]+)"`)

// internalObservabilityLogMessages are Warn/Error strings in
// internal/observability that are not operator runbook material (debug
// plumbing, shutdown internals). Anything else extracted from that tree
// must appear in docs/observability.md.
var internalObservabilityLogMessages = map[string]string{
	"readyz: failed to write response body": "handler write fallback after status is committed",
	"shutdown hook failed":                  "internal hook registry",
	"shutdown hook budget exceeded; abandoning any hooks still running": "internal hook registry",
	"otel sdk emitted an internal diagnostic on its own error/log channel; suppressed here because that channel is not audited for OTLP headers or endpoint credentials — see docs/observability.md": "SDK diagnostic suppression",
	"otel self-stats interval is non-positive; periodic fallback disabled": "config already re-defaults this; defensive ticker guard",
	"recovered panic in otel self-stats emitter": "safego around the emitter; covered by panic_recovered discussion",
	"OTLP metrics flush incomplete at shutdown":  "shutdown drain, not a standing operator symptom",
}

func goFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestObservabilityDoc_DocumentsObservabilityWarnAndErrorLogs(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "observability")
	doc := publicObservabilityDoc(t)
	seen := map[string]bool{}
	for _, path := range goFilesUnder(t, root) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range slogCallRE.FindAllSubmatch(src, -1) {
			msg := string(m[2])
			if seen[msg] {
				continue
			}
			seen[msg] = true
			if _, internal := internalObservabilityLogMessages[msg]; internal {
				continue
			}
			if !strings.Contains(doc, msg) {
				t.Errorf("docs/observability.md does not quote observability %s %q (from %s) — document it or add an internalObservabilityLogMessages exclusion", m[1], msg, path)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no slog.Warn/Error string literals under internal/observability")
	}
}

func TestObservabilityDoc_QuotesMetricsProviderBuildFailed(t *testing.T) {
	const msg = "metrics provider build failed"
	src := readRepoFile(t, "cmd", "server", "main.go")
	if !strings.Contains(src, `"`+msg+`"`) {
		t.Fatalf("cmd/server/main.go no longer logs %q", msg)
	}
	if !strings.Contains(operatorRunbookSection(t, publicObservabilityDoc(t)), msg) {
		t.Errorf("Operator Runbook does not quote %q", msg)
	}
}

func TestObservabilityDoc_DocumentsPanicMetricScope(t *testing.T) {
	runbook := operatorRunbookSection(t, publicObservabilityDoc(t))
	for _, needle := range []string{"safego", "tokenexchange", "event=\"panic_recovered\""} {
		if !strings.Contains(runbook, needle) && !strings.Contains(publicObservabilityDoc(t), needle) {
			t.Errorf("docs/observability.md does not explain panic-metric scope using %q (panics.go: logs without mcp_panic_recovered_total)", needle)
		}
	}
}
