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

Not part of the Metrics section — a parser bug that keeps reading past the
section boundary would pick up a row here if one existed.
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

func TestParseObservabilityDoc_NoTableRows(t *testing.T) {
	doc := "## Metrics — [x]\n\n> _live: none._\n>\n> _not yet: none._\n\n## Audit Trail\n"
	_, err := parseObservabilityDoc(doc)
	if err == nil {
		t.Fatal("expected an error (no metric tables found), got nil")
	}
}
