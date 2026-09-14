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
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fixture is a minimal but structurally faithful stand-in for the real
// docs/observability.md: the same heading shape, the same two-paragraph
// blockquote, and a couple of metric tables including the two edge cases the
// real doc has today (a "none" label cell and a "same label set, minus `x`"
// cross-reference). Kept independent of the real doc on purpose, so a future
// edit to docs/observability.md can't accidentally make this test vacuous.
const fixture = `# Fixture

## Metrics — [Planned, with exceptions]

> _Status: fixture. Wired and emitted today: ` + "`mcp_alpha_total`" + `,
> ` + "`mcp_beta_total`" + `. Assume any other metric below is not yet emitted._
>
> **One metric group below is documented but not emitted by any build yet**:
> ` + "`mcp_gamma_total`" + `.

### Alpha and Beta

| Metric | Type | Labels | Basis |
|---|---|---|---|
| ` + "`mcp_alpha_total`" + ` | Counter | ` + "`tool`, `broker`" + ` | Solace |
| ` + "`mcp_alpha_duration_seconds`" + ` | Histogram | same label set, minus ` + "`broker`" + ` | Solace |
| ` + "`mcp_beta_total`" + ` | Counter | none | Solace |

### Gamma — [Not yet emitted]

| Metric | Type | Labels | Basis |
|---|---|---|---|
| ` + "`mcp_gamma_total`" + ` | Counter | ` + "`reason`" + ` | Solace |

## Audit Trail

A real table, in the same shape a metric table would be, so a parser bug
that keeps reading past the section boundary actually has something to pick
up and get caught by TestParseObservabilityDoc's assertion that
` + "`mcp_outside_metrics_section_total`" + ` never appears in the parsed inventory.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| ` + "`mcp_outside_metrics_section_total`" + ` | Counter | none | Solace |
`

func TestParseObservabilityDoc(t *testing.T) {
	inv, err := parseObservabilityDoc(fixture)
	if err != nil {
		t.Fatalf("parseObservabilityDoc: %v", err)
	}

	wantMetrics := map[string]docMetric{
		"mcp_alpha_total":            {name: "mcp_alpha_total", labels: []string{"broker", "tool"}},
		"mcp_alpha_duration_seconds": {name: "mcp_alpha_duration_seconds", labels: []string{"tool"}},
		"mcp_beta_total":             {name: "mcp_beta_total", labels: nil},
		"mcp_gamma_total":            {name: "mcp_gamma_total", labels: []string{"reason"}},
	}
	if !reflect.DeepEqual(inv.metrics, wantMetrics) {
		t.Errorf("metrics = %+v, want %+v", inv.metrics, wantMetrics)
	}

	wantLive := map[string]bool{"mcp_alpha_total": true, "mcp_beta_total": true}
	if !reflect.DeepEqual(inv.liveClaimed, wantLive) {
		t.Errorf("liveClaimed = %v, want %v", inv.liveClaimed, wantLive)
	}

	if _, ok := inv.metrics["mcp_outside_metrics_section_total"]; ok {
		t.Error("a table under ## Audit Trail was parsed as if it were still in the Metrics section — the section boundary isn't being respected")
	}

	wantNotYet := map[string]bool{"mcp_gamma_total": true}
	if !reflect.DeepEqual(inv.notYetClaimed, wantNotYet) {
		t.Errorf("notYetClaimed = %v, want %v", inv.notYetClaimed, wantNotYet)
	}
}

// TestParseObservabilityDoc_AgainstRealDoc is the same parser run against the
// actual docs/observability.md, asserting only that it succeeds and finds a
// plausible amount of content — not pinning exact names, so this test
// doesn't need updating every time a story adds a metric. It exists so a
// doc restructure that breaks the parser's structural assumptions (the
// heading, the blockquote shape, the table header) is caught here, with a
// clear parser error, rather than surfacing as a confusing failure in
// TestObservabilityDocMatchesRegistry.
func TestParseObservabilityDoc_AgainstRealDoc(t *testing.T) {
	raw := readObservabilityDoc(t)
	inv, err := parseObservabilityDoc(raw)
	if err != nil {
		t.Fatalf("parseObservabilityDoc(docs/observability.md): %v", err)
	}
	if len(inv.metrics) < 10 {
		t.Errorf("parsed only %d metrics from the real doc, expected considerably more — the parser may be under-matching", len(inv.metrics))
	}
	if len(inv.liveClaimed) == 0 {
		t.Error("parsed zero live-claimed metric names from the real doc's blockquote")
	}
}

func TestExtractNames(t *testing.T) {
	got := extractNames("see `mcp_a_total` and `mcp_b_total`, but not `tool` or `mcp_a_total` again")
	want := map[string]bool{"mcp_a_total": true, "mcp_b_total": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractNames() = %v, want %v", got, want)
	}
}

func TestSubtract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		all, remove []string
		want        []string
	}{
		{name: "removes one", all: []string{"a", "b", "c"}, remove: []string{"b"}, want: []string{"a", "c"}},
		{name: "removes none matching", all: []string{"a", "b"}, remove: []string{"z"}, want: []string{"a", "b"}},
		{name: "removes all", all: []string{"a"}, remove: []string{"a"}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := subtract(tc.all, tc.remove)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("subtract(%v, %v) = %v, want %v", tc.all, tc.remove, got, tc.want)
			}
		})
	}
}

func TestParseMetricTables_UnknownCrossReferenceSilentlyDegrades(t *testing.T) {
	bad := strings.Replace(fixture, "same label set, minus `broker`", "same label set, minus `nonexistent`", 1)
	inv, err := parseObservabilityDoc(bad)
	if err != nil {
		t.Fatalf("parseObservabilityDoc: %v", err)
	}
	// "minus `nonexistent`" against {tool, broker} silently no-ops rather
	// than erroring — subtract() only removes what it finds — so this
	// documents that behavior rather than asserting a parse failure: a typo
	// in a "minus" clause degrades to "resolves to the full label set",
	// which the label-key diff against the live registry would then catch
	// as a mismatch. Confirm that degradation is what actually happens.
	got := inv.metrics["mcp_alpha_duration_seconds"].labels
	want := []string{"broker", "tool"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestParseObservabilityDoc_MissingHeading(t *testing.T) {
	_, err := parseObservabilityDoc("# no metrics section here\n")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// validBlockquote satisfies parseLiveEnumeration on its own — both anchor
// phrases present, two paragraphs — so every case below that uses it fails
// (or doesn't) purely on the table-parsing defect it's actually testing,
// not on an incidental blockquote-shape mismatch.
const validBlockquote = "> _live: `mcp_a_total`, wired and emitted today._\n>\n> _not emitted by any build yet: none._\n\n"

// TestParseObservabilityDoc_ToleratesLeadingHTMLComment locks in that an
// HTML comment between the "## Metrics" heading and the blockquote (like
// the one documenting this section's own machine-parsed structure in the
// real doc) doesn't trip the "no blockquote found" error — a real
// regression caught while adding that comment to docs/observability.md, not
// a hypothetical.
func TestParseObservabilityDoc_ToleratesLeadingHTMLComment(t *testing.T) {
	doc := "## Metrics — [x]\n\n" +
		"<!-- a multi-line\n     HTML comment -->\n\n" +
		validBlockquote +
		"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n\n## Audit Trail\n"
	inv, err := parseObservabilityDoc(doc)
	if err != nil {
		t.Fatalf("parseObservabilityDoc: %v", err)
	}
	if _, ok := inv.metrics["mcp_a_total"]; !ok {
		t.Error("mcp_a_total not parsed — the HTML comment likely confused the blockquote/table detection")
	}
}

func TestParseObservabilityDoc_NoTableRows(t *testing.T) {
	doc := "## Metrics — [x]\n\n" + validBlockquote + "## Audit Trail\n"
	_, err := parseObservabilityDoc(doc)
	if err == nil {
		t.Fatal("expected an error (no metric tables found), got nil")
	}
}

// TestParseObservabilityDoc_StructuralErrors covers every remaining error
// return in parseObservabilityDoc/parseLiveEnumeration/parseMetricTables —
// the fail-loud-on-any-structural-surprise property is the whole design's
// safety net, so each way it can fire gets its own case rather than being
// asserted only by the doc comment above the function. Each case is built on
// validBlockquote so the defect under test is the only thing that can make
// it fail.
func TestParseObservabilityDoc_StructuralErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{
			name: "no closing heading after Metrics",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n",
		},
		{
			name: "no blockquote after the heading",
			doc: "## Metrics — [x]\n\nNo blockquote here.\n\n" +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n\n## Audit Trail\n",
		},
		{
			name: "blockquote has only a live paragraph, no not-yet paragraph",
			doc: "## Metrics — [x]\n\n> _live: `mcp_a_total`, wired and emitted today._\n\n" +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n\n## Audit Trail\n",
		},
		{
			name: "table header not followed by a separator",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis |\n| `mcp_a_total` | Counter | none | Solace |\n\n## Audit Trail\n",
		},
		{
			name: "row doesn't match the expected shape",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\nmcp_a_total, Counter, none, Solace\n\n## Audit Trail\n",
		},
		{
			name: "same label set with no preceding row",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | same label set | Solace |\n\n## Audit Trail\n",
		},
		{
			name: "same metric name in two tables",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n\n" +
				"| Metric | Type | Labels | Basis |\n|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace |\n\n## Audit Trail\n",
		},
		{
			name: "table header near-miss (extra column) is rejected, not silently skipped",
			doc: "## Metrics — [x]\n\n" + validBlockquote +
				"| Metric | Type | Labels | Basis | Status |\n|---|---|---|---|---|\n| `mcp_a_total` | Counter | none | Solace | Live |\n\n## Audit Trail\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseObservabilityDoc(tc.doc)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			t.Logf("got expected error: %v", err)
		})
	}
}
