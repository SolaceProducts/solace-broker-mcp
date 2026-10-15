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

// SOL-154609: the flag-table counterpart to SOL-154238's
// TestObservabilityDocMatchesRegistry (cmd/server). It diffs each documented
// flag table (docFlagTables: the Feature Switches table in
// docs/observability.md and the Flag Defaults at GA table in
// docs/_internal/observability-schema-review.md) against the OBS_* capability
// flags this package defines, in both directions, and checks each documented
// default against obsFlagDefaults (observability_test.go).
//
// The flag inventory is read from the AST of every non-test Go file in this
// package — every string literal whose value is an OBS_*_ENABLED name, however
// the flag is declared or named — not from a hand-kept list, so a new flag
// fails this test until it is documented. The KA's per-capability flag table
// (Confluence) is outside what CI can read and is not checked.
package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// obsFlagPattern is what counts as a capability flag name, in code and in a
// doc table alike.
const obsFlagPattern = `OBS_[A-Z0-9_]+_ENABLED`

// obsFlagRE matches a code string literal's value that is a flag name.
var obsFlagRE = regexp.MustCompile(`^` + obsFlagPattern + `$`)

// flagCellRE matches a flag-name cell: one backticked OBS_*_ENABLED name.
var flagCellRE = regexp.MustCompile("^`(" + obsFlagPattern + ")`$")

// delimiterCellRE matches one cell of a Markdown table's delimiter row: ---,
// :---, ---:, or :---:.
var delimiterCellRE = regexp.MustCompile(`^:?-+:?$`)

// docFlagTable locates one documented flag table: the first table under
// heading in the repo-relative file path, whose first two columns are
// nameColumn and Default.
type docFlagTable struct {
	path       string
	heading    string
	nameColumn string
}

// label names the table in failure messages.
func (d docFlagTable) label() string {
	return d.path + " " + strings.TrimLeft(d.heading, "# ")
}

// docFlagTables are the tables TestObservabilityDocFlagTableMatchesCode checks.
var docFlagTables = []docFlagTable{
	{path: "docs/observability.md", heading: "## Feature Switches", nameColumn: "Variable"},
	{path: "docs/_internal/observability-schema-review.md", heading: "### Flag Defaults at GA", nameColumn: "Flag"},
}

// splitTableRow splits "| a | b |" into its trimmed cells.
func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// parseFlagTable returns env var name -> Default cell text (with surrounding
// backticks removed) from the first table under tbl.heading, within that
// heading's section (up to the next heading of the same or a higher level). It
// errors rather than returning an empty map when the structure is missing, so
// a doc rewrite cannot make the check pass vacuously.
func parseFlagTable(doc string, tbl docFlagTable) (map[string]string, error) {
	name := tbl.label()
	headingRE := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tbl.heading) + `[ \t]*$`)
	loc := headingRE.FindStringIndex(doc)
	if loc == nil {
		return nil, fmt.Errorf("%s: no %q heading", tbl.path, tbl.heading)
	}
	level := len(tbl.heading) - len(strings.TrimLeft(tbl.heading, "#"))
	nextSectionRE := regexp.MustCompile(fmt.Sprintf(`(?m)^#{1,%d} `, level))
	section := doc[loc[1]:]
	if next := nextSectionRE.FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}

	lines := strings.Split(section, "\n")
	header := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "|") {
			header = i
			break
		}
	}
	if header < 0 {
		return nil, fmt.Errorf("%s: no table under the heading", name)
	}
	cols := splitTableRow(lines[header])
	if len(cols) < 2 || cols[0] != tbl.nameColumn || cols[1] != "Default" {
		return nil, fmt.Errorf("%s: table header must start with | %s | Default |, got %q", name, tbl.nameColumn, lines[header])
	}

	// Markdown only renders a table when the header is followed by a delimiter
	// row with the same number of cells.
	if header+1 >= len(lines) {
		return nil, fmt.Errorf("%s: no delimiter row under the table header", name)
	}
	delim := splitTableRow(lines[header+1])
	if len(delim) != len(cols) {
		return nil, fmt.Errorf("%s: delimiter row %q has %d cells, header has %d", name, lines[header+1], len(delim), len(cols))
	}
	for _, c := range delim {
		if !delimiterCellRE.MatchString(c) {
			return nil, fmt.Errorf("%s: malformed delimiter row %q", name, lines[header+1])
		}
	}

	flags := map[string]string{}
	for _, l := range lines[header+2:] {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			break
		}
		cells := splitTableRow(l)
		if len(cells) < 2 {
			return nil, fmt.Errorf("%s: malformed row %q", name, l)
		}
		m := flagCellRE.FindStringSubmatch(cells[0])
		if m == nil {
			return nil, fmt.Errorf("%s: row %q: %s cell is not a backticked OBS_*_ENABLED name", name, l, tbl.nameColumn)
		}
		if _, dup := flags[m[1]]; dup {
			return nil, fmt.Errorf("%s: %s has more than one row", name, m[1])
		}
		flags[m[1]] = strings.Trim(cells[1], "`")
	}
	if len(flags) == 0 {
		return nil, fmt.Errorf("%s: table has no rows", name)
	}
	return flags, nil
}

// codeObsFlags returns, sorted and deduplicated, every string literal in the
// non-test Go files of dir whose value is an OBS_*_ENABLED name, whatever the
// enclosing const or call. A const named envObs* must hold such a literal
// itself: one that is computed or has no explicit value would hide its flag.
func codeObsFlags(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, err
		}
		var bad error
		ast.Inspect(f, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				for i, id := range node.Names {
					if !strings.HasPrefix(id.Name, "envObs") {
						continue
					}
					if i >= len(node.Values) {
						bad = fmt.Errorf("%s: %s has no explicit value", n, id.Name)
						continue
					}
					if lit, ok := node.Values[i].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
						bad = fmt.Errorf("%s: %s is not a string literal", n, id.Name)
					}
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				if v, err := strconv.Unquote(node.Value); err == nil && obsFlagRE.MatchString(v) {
					seen[v] = true
				}
			}
			return true
		})
		if bad != nil {
			return nil, bad
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no OBS_*_ENABLED string literals in %s", dir)
	}
	flags := make([]string, 0, len(seen))
	for v := range seen {
		flags = append(flags, v)
	}
	sort.Strings(flags)
	return flags, nil
}

// derivedDefaultFlags names flags whose unset default is computed from other
// flags rather than fixed, along with why each is excluded from the literal
// default comparison. Their Default cell must stay prose: a `true` or `false`
// there would misstate the behavior. The behavior itself is pinned elsewhere.
var derivedDefaultFlags = map[string]string{
	envObsAuthFailureCounterEnabled: "follows MetricsProviderEnabled when unset — pinned by TestObservability_AuthFailureCounter_FollowsProviderWhenUnset",
}

// checkFlagTable returns one message per disagreement between the flags in
// code, their expected defaults (obsFlagDefaults), and the doc table named by
// label.
func checkFlagTable(code []string, doc map[string]string, label string) []string {
	defaults := map[string]bool{}
	for _, d := range obsFlagDefaults {
		defaults[d.env] = d.want
	}

	var problems []string
	inCode := map[string]bool{}
	for _, name := range code {
		inCode[name] = true
		want, hasDefault := defaults[name]
		if !hasDefault {
			problems = append(problems, fmt.Sprintf("%s is defined in code but has no entry in obsFlagDefaults (observability_test.go) — add its expected default", name))
		}
		got, documented := doc[name]
		if !documented {
			problems = append(problems, fmt.Sprintf("%s is defined in code but has no row in the %s table — add one", name, label))
			continue
		}
		if reason, derived := derivedDefaultFlags[name]; derived {
			if got == "" || got == "true" || got == "false" {
				problems = append(problems, fmt.Sprintf("%s: %s says default %q, but its default is derived (%s) — describe it in prose", name, label, got, reason))
			}
			continue
		}
		if hasDefault && got != strconv.FormatBool(want) {
			problems = append(problems, fmt.Sprintf("%s: %s says default %q, code default is %v", name, label, got, want))
		}
	}
	for name := range doc {
		if !inCode[name] {
			problems = append(problems, fmt.Sprintf("%s documents %s, but no OBS_*_ENABLED literal in internal/config defines it — remove the row or add the flag", label, name))
		}
	}
	for name := range derivedDefaultFlags {
		if !inCode[name] {
			problems = append(problems, fmt.Sprintf("derivedDefaultFlags names %s, but no OBS_*_ENABLED literal in internal/config defines it — remove the stale exclusion", name))
		}
	}
	sort.Strings(problems)
	return problems
}

// TestObservabilityDocFlagTableMatchesCode fails when any table in
// docFlagTables and this package's OBS_* flags disagree: a flag with no row, a
// row with no flag, or a documented default that is not the code's.
func TestObservabilityDocFlagTableMatchesCode(t *testing.T) {
	code, err := codeObsFlags(".")
	if err != nil {
		t.Fatalf("codeObsFlags: %v", err)
	}
	for _, tbl := range docFlagTables {
		t.Run(tbl.label(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(tbl.path)))
			if err != nil {
				t.Fatalf("read %s: %v", tbl.path, err)
			}
			doc, err := parseFlagTable(string(raw), tbl)
			if err != nil {
				t.Fatalf("parseFlagTable: %v", err)
			}
			for _, p := range checkFlagTable(code, doc, tbl.label()) {
				t.Error(p)
			}
		})
	}
}

func TestParseFlagTable(t *testing.T) {
	featureSwitches := docFlagTables[0]
	const good = "# Doc\n\n## Feature Switches\n\nIntro.\n\n" +
		"| Variable | Default | When `true` |\n|---|---:|---|\n" +
		"| `OBS_A_ENABLED` | `true` | x |\n" +
		"| `OBS_B_ENABLED` | follows the two metrics flags | y |\n" +
		"\n### Sub\n\n| Variable | Default |\n|---|---|\n| `OBS_C_ENABLED` | `false` |\n" +
		"\n## Next\n"

	got, err := parseFlagTable(good, featureSwitches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"OBS_A_ENABLED": "true", "OBS_B_ENABLED": "follows the two metrics flags"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (only the first table under the heading)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	errCases := map[string]string{
		"no heading":            "## Other\n\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"no table":              "## Feature Switches\n\nProse only.\n\n## Next\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"wrong header":          "## Feature Switches\n\n| Name | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"no rows":               "## Feature Switches\n\n| Variable | Default |\n|---|---|\n\nText.\n",
		"bad variable":          "## Feature Switches\n\n| Variable | Default |\n|---|---|\n| OBS_A_ENABLED | `true` |\n",
		"duplicate row":         "## Feature Switches\n\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n| `OBS_A_ENABLED` | `false` |\n",
		"malformed row":         "## Feature Switches\n\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` |\n",
		"heading suffix":        "## Feature Switches Old\n\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"header is last line":   "## Feature Switches\n\n| Variable | Default |",
		"no delimiter row":      "## Feature Switches\n\n| Variable | Default |\n| not a delimiter | x |\n| `OBS_A_ENABLED` | `true` |\n",
		"delimiter too few":     "## Feature Switches\n\n| Variable | Default |\n|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"delimiter too many":    "## Feature Switches\n\n| Variable | Default |\n|---|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"delimiter empty cell":  "## Feature Switches\n\n| Variable | Default |\n|---||\n| `OBS_A_ENABLED` | `true` |\n",
		"delimiter dash inside": "## Feature Switches\n\n| Variable | Default |\n|---|--x--|\n| `OBS_A_ENABLED` | `true` |\n",
	}
	for name, doc := range errCases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFlagTable(doc, featureSwitches); err == nil {
				t.Error("expected an error")
			}
		})
	}

	// Every alignment form Markdown accepts in a delimiter cell, padded or not.
	const aligned = "## Feature Switches\n\n| Variable | Default | A | B |\n" +
		"| --- | :--- | ---: | :---: |\n| `OBS_A_ENABLED` | `true` | x | y |\n"
	if _, err := parseFlagTable(aligned, featureSwitches); err != nil {
		t.Errorf("aligned delimiter row: unexpected error: %v", err)
	}

	// A "###" table's section ends at the next "###" or "##", not at "####",
	// and its name column is the table's own.
	gaDefaults := docFlagTables[1]
	const ga = "## Status\n\n### Flag Defaults at GA\n\n" +
		"| Flag | Default | Why |\n|---|---|---|\n| `OBS_A_ENABLED` | `true` | x |\n" +
		"\n#### Detail\n\nMore.\n\n### Next\n\n| Flag | Default |\n|---|---|\n| `OBS_B_ENABLED` | `false` |\n"
	got, err = parseFlagTable(ga, gaDefaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got["OBS_A_ENABLED"] != "true" {
		t.Errorf("got %v, want only OBS_A_ENABLED=true", got)
	}
	gaErrCases := map[string]string{
		"table only after next ###": "### Flag Defaults at GA\n\nProse.\n\n### Next\n\n| Flag | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"table only after next ##":  "### Flag Defaults at GA\n\nProse.\n\n## Next\n\n| Flag | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
		"Variable header":           "### Flag Defaults at GA\n\n| Variable | Default |\n|---|---|\n| `OBS_A_ENABLED` | `true` |\n",
	}
	for name, doc := range gaErrCases {
		t.Run("ga "+name, func(t *testing.T) {
			if _, err := parseFlagTable(doc, gaDefaults); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestCheckFlagTable(t *testing.T) {
	code := make([]string, 0, len(obsFlagDefaults))
	matching := map[string]string{}
	for _, d := range obsFlagDefaults {
		code = append(code, d.env)
		matching[d.env] = strconv.FormatBool(d.want)
	}
	matching[envObsAuthFailureCounterEnabled] = "follows the two metrics flags"
	clone := func() map[string]string {
		m := make(map[string]string, len(matching))
		for k, v := range matching {
			m[k] = v
		}
		return m
	}

	const label = "docs/observability.md Feature Switches"
	if p := checkFlagTable(code, matching, label); len(p) != 0 {
		t.Fatalf("matching doc reported problems: %v", p)
	}
	reworded := clone()
	reworded[envObsAuthFailureCounterEnabled] = "on when either metrics egress is on"
	if p := checkFlagTable(code, reworded, label); len(p) != 0 {
		t.Errorf("rewording a derived default's prose reported problems: %v", p)
	}
	withoutAuth := func(names []string) []string {
		var out []string
		for _, n := range names {
			if n != envObsAuthFailureCounterEnabled {
				out = append(out, n)
			}
		}
		return out
	}

	cases := []struct {
		name string
		code []string
		doc  func() map[string]string
		want string
	}{
		{"flag missing from doc", code, func() map[string]string {
			m := clone()
			delete(m, envObsTracingEnabled)
			return m
		}, "OBS_TRACING_ENABLED is defined in code but has no row"},
		{"doc row with no flag", code, func() map[string]string {
			m := clone()
			m["OBS_PANIC_RECOVERY_ENABLED"] = "true"
			return m
		}, "documents OBS_PANIC_RECOVERY_ENABLED, but no OBS_*_ENABLED literal"},
		{"default mismatch", code, func() map[string]string {
			m := clone()
			m[envObsCorrelationIDEnabled] = "false"
			return m
		}, `OBS_CORRELATION_ID_ENABLED: docs/observability.md Feature Switches says default "false"`},
		{"code flag with no expected default", append(append([]string{}, code...), "OBS_NEW_ENABLED"), func() map[string]string {
			m := clone()
			m["OBS_NEW_ENABLED"] = "false"
			return m
		}, "OBS_NEW_ENABLED is defined in code but has no entry in obsFlagDefaults"},
		{"derived default documented as a bool", code, func() map[string]string {
			m := clone()
			m[envObsAuthFailureCounterEnabled] = "false"
			return m
		}, "OBS_AUTH_FAILURE_COUNTER_ENABLED: docs/observability.md Feature Switches says default \"false\", but its default is derived"},
		{"derived default left empty", code, func() map[string]string {
			m := clone()
			m[envObsAuthFailureCounterEnabled] = ""
			return m
		}, "OBS_AUTH_FAILURE_COUNTER_ENABLED: docs/observability.md Feature Switches says default \"\", but its default is derived"},
		{"stale derived exclusion", withoutAuth(code), func() map[string]string {
			m := clone()
			delete(m, envObsAuthFailureCounterEnabled)
			return m
		}, "derivedDefaultFlags names OBS_AUTH_FAILURE_COUNTER_ENABLED, but no OBS_*_ENABLED literal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := checkFlagTable(c.code, c.doc(), label)
			if len(p) != 1 || !strings.Contains(p[0], c.want) {
				t.Errorf("problems = %v, want exactly one containing %q", p, c.want)
			}
		})
	}
}

func TestCodeObsFlags(t *testing.T) {
	got, err := codeObsFlags(".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(obsFlagDefaults) {
		t.Errorf("found %d OBS_*_ENABLED literals %v, obsFlagDefaults has %d", len(got), got, len(obsFlagDefaults))
	}

	// pkg writes files into a fresh directory and returns it.
	pkg := func(files map[string]string) string {
		dir := t.TempDir()
		for name, src := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	// Found by value, in any file, under any name, and once each; not in a
	// test file, a comment, or a near-miss string.
	got, err = codeObsFlags(pkg(map[string]string{
		"a.go":      "package x\n\nconst anyName = \"OBS_A_ENABLED\"\n",
		"b.go":      "package x\n\n// OBS_COMMENT_ENABLED\nvar _ = f(\"OBS_B_ENABLED\", \"OBS_A_ENABLED\", \"OBS_X_ENABLED_SUFFIX\", \"obs_y_enabled\")\n\nfunc f(...string) bool { return false }\n",
		"c_test.go": "package x\n\nconst envObsT = \"OBS_T_ENABLED\"\n",
		"notes.txt": "OBS_TXT_ENABLED\n",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"OBS_A_ENABLED", "OBS_B_ENABLED"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}

	errCases := map[string]map[string]string{
		"no flags":           {"none.go": "package x\nconst other = \"X\"\n"},
		"computed envObs*":   {"nonlit.go": "package x\nconst base = \"OBS_\"\nconst envObsX = base + \"X_ENABLED\"\n"},
		"envObs* no value":   {"implicit.go": "package x\nconst (\n\tenvObsA = \"OBS_A_ENABLED\"\n\tenvObsB\n)\n"},
		"unparseable source": {"broken.go": "package x\nconst = \n"},
	}
	for name, files := range errCases {
		t.Run(name, func(t *testing.T) {
			if _, err := codeObsFlags(pkg(files)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
