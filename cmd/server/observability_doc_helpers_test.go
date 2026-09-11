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

// SOL-154238 (Story 14 gap): parses the metric inventory out of
// docs/observability.md so observability_doc_test.go can diff it against a
// live Prometheus registry. Deliberately a _test.go file with no Test
// functions of its own — this is pure CI/test tooling with no reason to
// exist in the shipped server binary, so it stays out of go build entirely
// rather than just going unused there. observability_doc_parser_test.go
// unit-tests it against small fabricated fixtures, independent of the real
// doc's future edits.
//
// Scope: metrics only. Audit fields and span attributes have their own
// tables in the same document; this ticket's AC is metrics names and label
// keys, not those.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// docMetric is one row parsed from a "| Metric | Type | Labels | Basis |"
// table: a metric name and its sorted label keys.
type docMetric struct {
	name   string
	labels []string // sorted, deduplicated
}

// docInventory is everything parseObservabilityDoc extracts from the
// Metrics section of docs/observability.md.
type docInventory struct {
	// metrics holds every row from every metric table in the Metrics
	// section, regardless of whether that table is currently live. This is
	// "documented", full stop — table membership alone proves it, no status
	// tag required.
	metrics map[string]docMetric

	// liveClaimed is the set of names the doc's own "wired and emitted
	// today" paragraph enumerates. This is the doc's *only* authoritative
	// statement of "should be live right now" — the Metrics section's own
	// stated default is "assume any other metric below is not yet emitted",
	// so absence from this set means "planned", not "undocumented".
	liveClaimed map[string]bool

	// notYetClaimed is the set of names the doc's companion paragraph
	// explicitly calls out as "documented but not emitted by any build
	// yet". Used only as a cross-check: a name in both liveClaimed and
	// notYetClaimed is a doc-internal contradiction, not a registry
	// question.
	notYetClaimed map[string]bool
}

var (
	// metricsSectionHeadingRE matches the "## Metrics" heading that opens
	// the section this parser cares about. It carries a status tag
	// ("— [Planned, with exceptions]") that varies over time, so the match
	// is deliberately loose — anchored on "## Metrics" at the start of a
	// line, not on the tag text.
	metricsSectionHeadingRE = regexp.MustCompile(`(?m)^## Metrics\b`)
	// nextTopLevelHeadingRE finds the next "## " heading after the Metrics
	// section starts, which closes the section off. docs/observability.md
	// keeps every metric table between "## Metrics" and the next "## "
	// heading (currently "## Audit Trail") — verified by inspection, not
	// assumed.
	nextTopLevelHeadingRE = regexp.MustCompile(`(?m)^## `)

	tableHeaderRE = regexp.MustCompile(`(?m)^\| *Metric *\| *Type *\| *Labels *\| *Basis *\|$`)
	tableSepRE    = regexp.MustCompile(`^\|(?:-+\|)+$`)
	// tableRowRE splits a "| `name` | Type | Labels | Basis |" row into its
	// four cells. Only the first (name) and third (labels) cells are used.
	tableRowRE = regexp.MustCompile("^\\| *`([a-zA-Z0-9_.]+)` *\\|([^|]*)\\|([^|]*)\\|([^|]*)\\|$")

	backtickTokenRE = regexp.MustCompile("`([a-zA-Z0-9_.]+)`")
	// sameLabelSetRE matches a Labels cell that cross-references the
	// preceding row instead of restating its label list — today's one
	// instance is mcp_semp_request_duration_seconds's "same label set,
	// minus `attempt`". Captures an optional "minus `x`, `y`, ..." clause.
	sameLabelSetRE = regexp.MustCompile(`(?i)^same label set(?:,\s*minus\s*(.+))?$`)
)

// parseObservabilityDoc extracts the metric inventory from raw
// docs/observability.md content. It returns an error — rather than an empty
// or partial result — whenever a structural assumption this parser depends
// on doesn't hold, so a doc restructure that breaks this parser fails the
// build loudly instead of silently passing an empty diff.
func parseObservabilityDoc(raw string) (*docInventory, error) {
	loc := metricsSectionHeadingRE.FindStringIndex(raw)
	if loc == nil {
		return nil, fmt.Errorf("no \"## Metrics\" heading found")
	}
	// loc[1] ends where the regex match ends ("## Metrics"), not where the
	// heading *line* ends — the rest of that line (a status tag like
	// "— [Planned, with exceptions]") still follows. Skip to the end of the
	// line so section starts fresh on the blockquote below it.
	section := raw[loc[1]:]
	if nl := strings.IndexByte(section, '\n'); nl >= 0 {
		section = section[nl+1:]
	}
	if end := nextTopLevelHeadingRE.FindStringIndex(section); end != nil {
		section = section[:end[0]]
	} else {
		return nil, fmt.Errorf("no top-level heading found after \"## Metrics\" to close the section")
	}

	liveClaimed, notYetClaimed, err := parseLiveEnumeration(section)
	if err != nil {
		return nil, err
	}

	metrics, err := parseMetricTables(section)
	if err != nil {
		return nil, err
	}

	return &docInventory{
		metrics:       metrics,
		liveClaimed:   liveClaimed,
		notYetClaimed: notYetClaimed,
	}, nil
}

// parseLiveEnumeration reads the blockquote immediately after the Metrics
// heading. It is written as two paragraphs separated by a bare "&gt;" line:
// the first says what is "wired and emitted today", the second says what is
// "documented but not emitted by any build yet". The split is on that blank
// blockquote line, not on the wording, so a rewrite of the sentences
// themselves doesn't break this parser — only a restructure of the
// blockquote into some other shape would, and that fails loudly below.
func parseLiveEnumeration(section string) (live, notYet map[string]bool, err error) {
	lines := strings.Split(section, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			start = i
			break
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		// Any non-blank, non-blockquote line before the blockquote starts
		// means the doc no longer opens the Metrics section with one.
		break
	}
	if start == -1 {
		return nil, nil, fmt.Errorf("no blockquote found immediately after the \"## Metrics\" heading")
	}

	var paragraphs [][]string
	var cur []string
	for i := start; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, ">") {
			break // blockquote ended
		}
		content := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		if content == "" {
			if len(cur) > 0 {
				paragraphs = append(paragraphs, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, content)
	}
	if len(cur) > 0 {
		paragraphs = append(paragraphs, cur)
	}
	if len(paragraphs) < 2 {
		return nil, nil, fmt.Errorf(
			"expected the Metrics-section blockquote to have at least two paragraphs "+
				"(a \"wired and emitted today\" list and a \"documented but not emitted\" list), found %d",
			len(paragraphs))
	}

	live = extractNames(strings.Join(paragraphs[0], " "))
	notYet = extractNames(strings.Join(paragraphs[1], " "))
	return live, notYet, nil
}

// extractNames pulls every backtick-quoted mcp_* token out of s.
func extractNames(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range backtickTokenRE.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if strings.HasPrefix(name, "mcp_") {
			out[name] = true
		}
	}
	return out
}

// parseMetricTables finds every "| Metric | Type | Labels | Basis |" table
// in section and returns one docMetric per row.
func parseMetricTables(section string) (map[string]docMetric, error) {
	lines := strings.Split(section, "\n")
	out := map[string]docMetric{}

	for i := 0; i < len(lines); i++ {
		if !tableHeaderRE.MatchString(lines[i]) {
			continue
		}
		if i+1 >= len(lines) || !tableSepRE.MatchString(strings.TrimSpace(lines[i+1])) {
			return nil, fmt.Errorf("line %d: \"| Metric | Type | Labels | Basis |\" header not followed by a separator row", i+1)
		}

		var lastLabels []string
		haveLast := false
		j := i + 2
		for ; j < len(lines); j++ {
			row := strings.TrimSpace(lines[j])
			if row == "" {
				break
			}
			m := tableRowRE.FindStringSubmatch(lines[j])
			if m == nil {
				return nil, fmt.Errorf("line %d: row in a metric table doesn't match the expected shape: %q", j+1, lines[j])
			}
			name := m[1]
			labelsCell := strings.TrimSpace(m[3])

			var labels []string
			switch {
			case labelsCell == "none":
				labels = nil
			case sameLabelSetRE.MatchString(labelsCell):
				if !haveLast {
					return nil, fmt.Errorf("line %d: %q references \"same label set\" but there is no preceding row in this table", j+1, name)
				}
				sub := sameLabelSetRE.FindStringSubmatch(labelsCell)
				minus := extractNamesOrdered(sub[1])
				labels = subtract(lastLabels, minus)
			default:
				labels = extractNamesOrdered(labelsCell)
			}

			sorted := append([]string(nil), labels...)
			sort.Strings(sorted)
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("line %d: %q appears in more than one metric table", j+1, name)
			}
			out[name] = docMetric{name: name, labels: sorted}

			lastLabels = labels
			haveLast = true
		}
		i = j
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("found no metric tables in the Metrics section at all")
	}
	return out, nil
}

// extractNamesOrdered pulls every backtick-quoted token out of s, in
// document order (unlike extractNames, which builds an unordered set) — the
// order matters for resolving a "minus `x`, `y`" clause against the
// preceding row's own label order.
func extractNamesOrdered(s string) []string {
	var out []string
	for _, m := range backtickTokenRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func subtract(all, remove []string) []string {
	skip := map[string]bool{}
	for _, r := range remove {
		skip[r] = true
	}
	var out []string
	for _, a := range all {
		if !skip[a] {
			out = append(out, a)
		}
	}
	return out
}
