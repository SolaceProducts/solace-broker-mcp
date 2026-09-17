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

// SOL-152092 (Story 37): TestGrafanaDashboardMatchesGoldenFile is the CI lint
// the ticket's own "Technical notes" asks for — a dashboard and a scrape
// cannot disagree, checked against Story 14's golden file (the authority on
// what a metric name and its label keys actually render as), not against
// docs/observability.md's prose. Unlike TestObservabilityDocMatchesRegistry
// (which scrapes a live registry), this test's ground truth is the committed
// golden file directly: the dashboard JSON is static, checked-in reference
// material, not something a running server produces, so there is no live
// registry to scrape here.
package main

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// dashboardPath and goldenFilePath are relative to cmd/server, two ".."
// segments up to the repo root — the same convention readObservabilityDoc
// (observability_doc_test.go) uses.
const (
	dashboardPath  = "../../deploy/grafana/solace-broker-mcp-overview.json"
	goldenFilePath = "../../internal/observability/metrics/testdata/metrics_golden.txt"
)

// universalLabels are labels legitimate on any histogram/summary family or
// any cross-series join, without being a per-family label the golden file's
// parse would surface: "job"/"instance" are Prometheus's own scrape/
// ingestion metadata, present on every series regardless of what this
// server emits; "le" is a structural part of every histogram's bucket
// samples, which expfmt.TextToMetricFamilies parses into Histogram.Bucket
// entries rather than into GetLabel() — so it never appears in goldenLabels'
// per-family label set even though every histogram family genuinely has it.
var universalLabels = map[string]bool{"job": true, "instance": true, "le": true}

// nonSchemaFamilies are metric families the dashboard legitimately
// references that the golden file does not cover, because the golden file
// is scoped to first-party mcp_* instruments only. scrapeLiveMCPFamilies
// (observability_doc_test.go) skips exactly these same prefixes for the same
// reason ("go_*, process_*, target_info: not first-party schema"). Hand
// -maintained here rather than derived: there is no single committed source
// for "every label target_info or the upstream Go/process collectors can
// carry" the way the golden file is that source for mcp_* names. Keep this
// in sync with whatever target_info/go_*/process_* series the dashboard
// actually queries — it is deliberately not "every label these families
// could ever carry," just the ones this dashboard uses, so an unused entry
// here would be dead weight, not a safety margin.
var nonSchemaFamilies = map[string]map[string]bool{
	// Only cloud_region: this dashboard's $service_name variable and its
	// panel joins deliberately key off "job" (a universalLabels entry, not
	// a target_info-specific one — see Finding 12 in the SOL-152092 plan
	// doc) rather than target_info's own service_name/service_instance_id
	// labels, because target_info does not carry those two on Prometheus's
	// OTLP-ingestion path (verified live). Don't add them back here on the
	// assumption they're "obviously" needed — that assumption is exactly
	// what this comment block above warns against.
	"target_info":                  labelSet("cloud_region"),
	"go_goroutines":                labelSet(),
	"go_memstats_heap_inuse_bytes": labelSet(),
	"process_cpu_seconds_total":    labelSet(),
}

func labelSet(labels ...string) map[string]bool {
	out := map[string]bool{}
	for _, l := range labels {
		out[l] = true
	}
	return out
}

// v1xExcludedMetrics are the metric names proposed for Stories 17 (retry
// outcome counter), 18 (broker pool gauges), and 28/29 (saturation events) —
// all v1.x. Story 37's AC is explicit that the GA dashboard ships no panels
// against these: they don't exist in any registry yet, so a panel built
// against one would import broken. Sourced from
// broker-mcp-obs-tel-sol-150251-Stories.md (discovery), not the golden file:
// since these names aren't registered anywhere yet, TestGrafanaDashboard
// MatchesGoldenFile would already reject them as "unknown metric" — this
// list exists to give that specific mistake a much clearer failure message
// than a generic unknown-metric error, and to keep failing even if one of
// these names is later added to the golden file for an unrelated reason
// before its own panel work formally lands.
var v1xExcludedMetrics = []string{
	"mcp_semp_retry_total",
	"mcp_broker_pool_in_flight",
	"mcp_broker_pool_max_concurrent",
	"mcp_broker_pool_max_idle_conns_per_host",
	"mcp_saturation_total",
}

// histogramSuffixes are the sample-name suffixes a histogram family's TYPE
// line does not carry but its actual scrape samples (and this dashboard's
// PromQL) do — stripped to recover the family name the golden file
// registers the family under.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// metricNameRe finds every mcp_*/go_*/process_*/target_info identifier in a
// PromQL expression. Regex over the expression text, not a PromQL parser:
// proportionate to what this lint needs (does the dashboard name something
// real?), not a full query-correctness checker.
//
// KEEP IN SYNC: mirrored by hand in test/e2e-dashboard/verify-panels.py
// (METRIC_NAME_RE) — the SOL-154545 nightly's own dashboard-JSON parser,
// independently implemented in Python rather than sharing this one. A
// change here (a new metric-name family) must be mirrored there too, or the
// static test and the live nightly can silently disagree about what a
// "supported" panel looks like.
var metricNameRe = regexp.MustCompile(`\b(?:mcp_[a-zA-Z0-9_]*|go_[a-zA-Z0-9_]*|process_[a-zA-Z0-9_]*|target_info)\b`)

// braceRe finds the contents of every {...} selector. Sufficient for this
// dashboard: none of its queries nest braces.
var braceRe = regexp.MustCompile(`\{([^{}]*)\}`)

// labelKeyRe pulls a label key out of a "key<op>" fragment inside a {...}
// selector (op is one of =, !=, =~, !~).
var labelKeyRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:=~|!~|!=|=)`)

// quotedStringRe matches a double-quoted PromQL string literal. Applied
// before labelKeyRe so a literal "=" inside a label VALUE (e.g. a regex
// value like outcome=~"foo=bar") can never be mistaken for another label
// key — labelKeyRe has no concept of quoting and would otherwise match
// "foo" there too.
var quotedStringRe = regexp.MustCompile(`"[^"]*"`)

// groupingRe finds PromQL aggregation/join grouping clauses (by/without/
// on/ignoring/group_left/group_right) and captures their parenthesized
// label list.
var groupingRe = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)

// labelValuesRe matches a Grafana template variable's label_values(metric,
// label) query — distinct syntax from a panel's PromQL selector, so it
// needs its own extraction rather than braceRe/groupingRe (which find
// nothing in a string that contains no "{" or "by(...)"-style clause).
//
// KEEP IN SYNC: mirrored by hand as LABEL_VALUES_RE in
// test/e2e-dashboard/verify-panels.py — see metricNameRe's comment above for
// why these two parsers exist independently rather than sharing one source.
var labelValuesRe = regexp.MustCompile(`^label_values\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$`)

// dashboardTarget, dashboardPanel, and templatingVar mirror only the subset
// of Grafana's dashboard-JSON shape this test needs: a title (for
// actionable failure messages) and every target's expr, recursing into
// row-nested panels (Grafana nests a collapsed row's panels under its own
// "panels" array); and each template variable's name and query.
type dashboardTarget struct {
	Expr string `json:"expr"`
}

type dashboardPanel struct {
	Title   string            `json:"title"`
	Targets []dashboardTarget `json:"targets"`
	Panels  []dashboardPanel  `json:"panels"`
}

type templatingVar struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

type dashboardDoc struct {
	Panels     []dashboardPanel `json:"panels"`
	Templating struct {
		List []templatingVar `json:"list"`
	} `json:"templating"`
}

// panelExpr pairs one panel target's expr with its panel's title, so a
// failure names the panel a dashboard author would actually go find.
type panelExpr struct {
	title string
	expr  string
}

func readDashboardRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("read %s: %v", dashboardPath, err)
	}
	return raw
}

func parseDashboardDoc(t *testing.T) dashboardDoc {
	t.Helper()
	var doc dashboardDoc
	if err := json.Unmarshal(readDashboardRaw(t), &doc); err != nil {
		t.Fatalf("parse %s: %v", dashboardPath, err)
	}
	return doc
}

func loadDashboardTargets(t *testing.T) []panelExpr {
	t.Helper()
	doc := parseDashboardDoc(t)

	var out []panelExpr
	var walk func([]dashboardPanel)
	walk = func(panels []dashboardPanel) {
		for _, p := range panels {
			for _, tgt := range p.Targets {
				if tgt.Expr != "" {
					out = append(out, panelExpr{title: p.Title, expr: tgt.Expr})
				}
			}
			if len(p.Panels) > 0 {
				walk(p.Panels)
			}
		}
	}
	walk(doc.Panels)
	return out
}

// loadTemplatingVars returns the dashboard's template-variable definitions.
// Their queries are checked separately from panel targets
// (TestGrafanaDashboardTemplatingMatchesGoldenFile) because label_values(...)
// is a distinct syntax a PromQL selector/grouping-clause regex does not
// recognize — a variable dropdown that silently returns empty is exactly
// the "customer gets a broken dashboard" failure mode this dashboard is
// committed to avoiding (Finding 12), so its query deserves the same
// golden-file check as an ordinary panel, not a weaker one.
func loadTemplatingVars(t *testing.T) []templatingVar {
	t.Helper()
	return parseDashboardDoc(t).Templating.List
}

// goldenLabels maps a metric family name (the golden file's TYPE-line name,
// e.g. "mcp_tool_invocation_duration_seconds" for a histogram — not any one
// suffixed sample name) to the set of label keys it carries, unioned across
// every sample of that family, plus every entry from nonSchemaFamilies.
func goldenLabels(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(goldenFilePath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenFilePath, err)
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse golden file: %v", err)
	}

	out := map[string]map[string]bool{}
	for name, fam := range families {
		labels := map[string]bool{}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = true
			}
		}
		out[name] = labels
	}
	for name, labels := range nonSchemaFamilies {
		out[name] = labels
	}
	return out
}

// resolveFamily maps a PromQL-referenced identifier back to the golden
// file's family name — direct match for a counter/gauge (whose golden name
// already IS its full name), or a histogram sample suffix stripped off.
func resolveFamily(name string, golden map[string]map[string]bool) (string, bool) {
	if _, ok := golden[name]; ok {
		return name, true
	}
	for _, suf := range histogramSuffixes {
		if base, ok := strings.CutSuffix(name, suf); ok {
			if _, ok := golden[base]; ok {
				return base, true
			}
		}
	}
	return "", false
}

// extractLabelKeys returns every label key referenced in expr, from both
// {...} selectors and aggregation/join grouping clauses ("$" template
// variables like "$broker" are not label keys and never match either
// regex's identifier shape, so they fall out for free).
func extractLabelKeys(expr string) map[string]bool {
	keys := map[string]bool{}
	for _, brace := range braceRe.FindAllStringSubmatch(expr, -1) {
		stripped := quotedStringRe.ReplaceAllString(brace[1], `""`)
		for _, lm := range labelKeyRe.FindAllStringSubmatch(stripped, -1) {
			if lm[1] != "__name__" {
				keys[lm[1]] = true
			}
		}
	}
	for _, grouping := range groupingRe.FindAllStringSubmatch(expr, -1) {
		for _, part := range strings.Split(grouping[1], ",") {
			if part = strings.TrimSpace(part); part != "" {
				keys[part] = true
			}
		}
	}
	return keys
}

func dedupeSorted(names []string) []string {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestGrafanaDashboardMatchesGoldenFile is Story 37's own technical-notes
// requirement: every metric name and label key the dashboard's panels
// reference must exist in Story 14's golden file (or the small, disclosed
// nonSchemaFamilies allow-list for target_info/go_*/process_*).
//
// Scoping, disclosed rather than silently assumed: a label key is checked
// against the UNION of every metric family referenced in the same target
// expr, not against one specific family. That catches the failure this test
// exists for — a name or label key that does not exist anywhere in the
// documented schema — without a full PromQL parser to attribute each label
// to its exact operand. It would not catch a label key that is real on some
// OTHER mcp_* family but wrongly applied to the family actually being
// queried (e.g. a stray "tool" label filter on a SEMP metric); that class of
// bug is a query-correctness error, not a schema-drift error, and is out of
// scope for what this lint claims to guarantee.
func TestGrafanaDashboardMatchesGoldenFile(t *testing.T) {
	golden := goldenLabels(t)
	targets := loadDashboardTargets(t)
	if len(targets) == 0 {
		t.Fatal("no panel targets found in the dashboard JSON — fixture path or panel-JSON shape assumption is wrong")
	}

	for _, tgt := range targets {
		names := dedupeSorted(metricNameRe.FindAllString(tgt.expr, -1))
		if len(names) == 0 {
			t.Errorf("panel %q: target expr references no mcp_*/go_*/process_*/target_info metric at all: %q", tgt.title, tgt.expr)
			continue
		}

		var families []string
		for _, n := range names {
			fam, ok := resolveFamily(n, golden)
			if !ok {
				t.Errorf("panel %q: %q is not in the golden file (nor the nonSchemaFamilies allow-list) — dashboard and schema have drifted", tgt.title, n)
				continue
			}
			families = append(families, fam)
		}

		for key := range extractLabelKeys(tgt.expr) {
			if universalLabels[key] {
				continue
			}
			found := false
			for _, fam := range families {
				if golden[fam][key] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("panel %q: label key %q is not a label on any of %v in the golden file (expr: %q)", tgt.title, key, families, tgt.expr)
			}
		}
	}
}

// TestGrafanaDashboardTemplatingMatchesGoldenFile is
// TestGrafanaDashboardMatchesGoldenFile's counterpart for the three
// dashboard variables ($service_name, $cloud_region, $broker): their
// label_values(metric, label) queries are schema-checked against the same
// golden file, the same way panel targets are. Kept separate from that test
// rather than folded in, because label_values(...) needs its own parser
// (labelValuesRe), not the brace/grouping regexes a panel's PromQL uses.
func TestGrafanaDashboardTemplatingMatchesGoldenFile(t *testing.T) {
	golden := goldenLabels(t)
	vars := loadTemplatingVars(t)
	if len(vars) == 0 {
		t.Fatal("no template variables found in the dashboard JSON — fixture path or templating-JSON shape assumption is wrong")
	}

	for _, v := range vars {
		m := labelValuesRe.FindStringSubmatch(strings.TrimSpace(v.Query))
		if m == nil {
			t.Errorf("variable %q: query %q is not a label_values(metric, label) call — this test only knows how to check that form; extend it rather than leaving a new form unchecked", v.Name, v.Query)
			continue
		}
		metric, label := m[1], m[2]
		fam, ok := resolveFamily(metric, golden)
		if !ok {
			t.Errorf("variable %q: %q is not in the golden file (nor the nonSchemaFamilies allow-list) — dashboard and schema have drifted", v.Name, metric)
			continue
		}
		if !universalLabels[label] && !golden[fam][label] {
			t.Errorf("variable %q: label key %q is not a label on %q in the golden file (query: %q) — this variable's dropdown would silently return empty", v.Name, label, fam, v.Query)
		}
	}
}

// TestGrafanaDashboardExcludesV1xMetrics guards the AC bullet that panels
// for retry outcomes, broker pool gauges, and saturation events must NOT
// ship in the GA dashboard (Stories 17, 18, 28, 29 are v1.x). Checked
// against the raw file text rather than parsed targets, deliberately: a
// v1.x metric name has no legitimate reason to appear ANYWHERE in this
// file — not in an expr, a title, or a description — so the stricter check
// costs nothing.
func TestGrafanaDashboardExcludesV1xMetrics(t *testing.T) {
	raw := string(readDashboardRaw(t))
	for _, excluded := range v1xExcludedMetrics {
		if strings.Contains(raw, excluded) {
			t.Errorf("dashboard references %q — Story 37's AC excludes Stories 17/18/28/29 (retry outcomes, broker pool gauges, saturation events) from the GA dashboard because those metrics do not exist yet", excluded)
		}
	}
}

// TestResolveFamilyAndExtractLabelKeys_DetectDrift is the committed regression
// guard the PR description referred to — during development, the schema
// checks above were proven to actually fail on an injected bad metric name
// and an injected bad label key by hand (editing the committed dashboard
// JSON, running `go test`, observing the failure, then reverting). That
// proved the *tests* catch drift, but left no permanent guard that
// `resolveFamily`/`extractLabelKeys` themselves keep catching it after a
// future edit to either. This test exercises both directly, against a small
// synthetic golden map, so a change that silently turns either into a
// no-op fails CI instead of only ever being caught by a repeat of that
// manual exercise.
func TestResolveFamilyAndExtractLabelKeys_DetectDrift(t *testing.T) {
	golden := map[string]map[string]bool{
		"mcp_tool_invocation_total": labelSet("broker", "outcome"),
	}

	t.Run("resolveFamily", func(t *testing.T) {
		if _, ok := resolveFamily("mcp_tool_invocation_total", golden); !ok {
			t.Error("a real, present family must resolve")
		}
		if _, ok := resolveFamily("mcp_totally_made_up_metric", golden); ok {
			t.Error("an unknown metric name must NOT resolve — this is the exact drift the lint exists to catch")
		}
		// A histogram's sample-suffixed name only resolves once its base
		// family is actually present in the golden map — not unconditionally.
		if _, ok := resolveFamily("mcp_tool_invocation_duration_seconds_bucket", golden); ok {
			t.Error("a histogram suffix must not resolve against a golden map that never registered the base family")
		}
		golden["mcp_tool_invocation_duration_seconds"] = labelSet("broker", "tool")
		if _, ok := resolveFamily("mcp_tool_invocation_duration_seconds_bucket", golden); !ok {
			t.Error("once the base histogram family is present, its _bucket-suffixed name must resolve to it")
		}
	})

	t.Run("extractLabelKeys", func(t *testing.T) {
		keys := extractLabelKeys(`sum by (outcome) (mcp_tool_invocation_total{broker=~"$broker", made_up_label="x"})`)
		for _, want := range []string{"outcome", "broker", "made_up_label"} {
			if !keys[want] {
				t.Errorf("extractLabelKeys(...) missing expected key %q from %v", want, keys)
			}
		}
		// made_up_label is present in golden's absence deliberately: this
		// confirms extraction finds a bad key at all, which is the
		// precondition for TestGrafanaDashboardMatchesGoldenFile's own
		// golden[fam][key] check to ever have something to reject.
		if golden["mcp_tool_invocation_total"]["made_up_label"] {
			t.Fatal("test setup bug: made_up_label must not be a real label in this synthetic golden map")
		}

		// A literal "=" inside a quoted label VALUE must not be mistaken
		// for another label key (flagged in PR review: a value like
		// outcome=~"foo=bar" would otherwise spuriously extract "foo").
		quoted := extractLabelKeys(`mcp_tool_invocation_total{outcome=~"foo=bar"}`)
		if quoted["foo"] {
			t.Errorf("extractLabelKeys(...) = %v, must not extract \"foo\" from inside a quoted value", quoted)
		}
		if !quoted["outcome"] {
			t.Errorf("extractLabelKeys(...) = %v, must still find the real \"outcome\" key", quoted)
		}
	})
}

// requiredACFamilies are the metric families Story 37's AC requires a panel
// for — one per signal named in the AC (Tool RED, active requests, SEMP
// RED, auth-failure breakdown, Go runtime, build info). Checked as "does
// the dashboard reference this family at all", deliberately coarse: the
// schema-drift tests above only ever check what a panel DOES reference
// against the golden file, so a panel silently deleted (or its query
// gutted) would shrink what gets checked and still pass every other test
// in this file. This one exists specifically to catch that.
var requiredACFamilies = []string{
	"mcp_tool_invocation_total",
	"mcp_tool_invocation_duration_seconds",
	"mcp_http_active_requests",
	"mcp_semp_request_total",
	"mcp_semp_request_duration_seconds",
	"mcp_auth_failure_total",
	"go_goroutines",
	"mcp_build_info",
}

func TestGrafanaDashboardCoversRequiredACPanels(t *testing.T) {
	raw := string(readDashboardRaw(t))
	for _, fam := range requiredACFamilies {
		if !strings.Contains(raw, fam) {
			t.Errorf("dashboard no longer references %q — Story 37's AC requires a panel for this family; either a panel was accidentally deleted, or this list needs updating alongside a deliberate, reviewed removal", fam)
		}
	}
	// The error-type breakdown is checked by its literal AC expression, not
	// just the family name, because the AC pins the exact query
	// (ADR-009) — "some panel mentions error_type" is not the same
	// guarantee as "the panel computes what the AC says it must". Checked
	// against the parsed target exprs (loadDashboardTargets), not raw file
	// bytes: raw bytes carry the JSON string encoding's escaped quotes
	// (`\"error\"`), which never matches a Go string literal containing a
	// bare `"`.
	const errorBreakdownExpr = `sum by (error_type) (rate(mcp_tool_invocation_total{outcome="error"`
	found := false
	for _, tgt := range loadDashboardTargets(t) {
		if strings.Contains(tgt.expr, errorBreakdownExpr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("dashboard is missing the AC-required error breakdown query (expected some panel's expr to contain %q)", errorBreakdownExpr)
	}
}
