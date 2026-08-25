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

// Command fidelity is the hard gate before any performance load run. It
// connects to a live MCP server, invokes every tool in buildChecks, and
// deep-equals the tool output against golden JSON captured earlier (with MCP
// pointed at the real broker).
//
// Exit codes:
//
//	0  all tools matched their golden
//	1  at least one tool diverged (or an error setting up the check)
//
// A divergent field is printed as "path: golden=<x> actual=<y>" so the
// caller can grep out exactly what drifted. No wiggle room; the plan says
// non-empty diff stops the run.
//
// Usage:
//
//	fidelity \
//	  -mcp-url http://localhost:9090 \
//	  -broker  my-broker \
//	  -vpn     default \
//	  -rdp     rdp_1 \
//	  -golden-dir test/performance/fidelity/golden
//
// Auth: reads MCP_DEV_TOKEN from env and sends it as Bearer if set. Leave
// unset when MCP is configured with mcp_client_auth.mode=disabled.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	mcpURL := flag.String("mcp-url", "http://localhost:9090", "MCP server URL")
	broker := flag.String("broker", "", "broker alias to invoke tools against (required)")
	vpn := flag.String("vpn", "default", "msgVpnName for the tools that take one (list-queues, list-rdps, get-rdp-status)")
	rdp := flag.String("rdp", "", "restDeliveryPointName for get-rdp-status (required). Must name the RDP the capture pinned — the run scripts read it from fixtures.manifest. Deliberately not defaulted: a default that disagreed with the capture would surface as a confusing diff or a mock miss instead of a clear error.")
	goldenDir := flag.String("golden-dir", "test/performance/fidelity/golden", "directory containing golden JSON files")
	exclusionsPath := flag.String("exclusions", "", "path to a plain-text file of dotted paths (one per line, # comments allowed) that exact-mode diff should skip — e.g. broker uptime, which advances between the canned and golden captures. Default: <golden-dir>/../exclusions.txt if it exists.")
	capture := flag.Bool("capture", false, "capture mode: overwrite golden files with the tools' current output instead of comparing (use when MCP is pointed at the real broker)")
	shape := flag.Bool("shape", false, "compare shape only (same keys, same types, same array lengths) — ignore scalar values. Use when you want structural fidelity without brittle drift on live metrics.")
	timeout := flag.Duration("timeout", 30*time.Second, "overall deadline")
	flag.Parse()

	if *broker == "" {
		fmt.Fprintln(os.Stderr, "fidelity: -broker is required")
		os.Exit(1)
	}
	if *rdp == "" {
		fmt.Fprintln(os.Stderr, "fidelity: -rdp is required (the RDP name the capture pinned; ./fixtures-manifest.sh rdp prints it)")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *capture {
		if err := captureGoldens(ctx, *mcpURL, *broker, *vpn, *rdp, *goldenDir); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("OK: goldens written to " + *goldenDir)
		return
	}

	exclusions, err := loadExclusions(*exclusionsPath, *goldenDir, *shape)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}

	if err := run(ctx, *mcpURL, *broker, *vpn, *rdp, *goldenDir, *shape, exclusions); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: all tools matched their golden")
}

// loadExclusions reads a plain-text exclusions file — one dotted path per
// line, "#" comments and blank lines ignored — and returns the set of
// paths whose diff should be skipped. If explicit is empty, tries
// "<goldenDir>/../exclusions.txt" and treats absence as an empty set (so
// hand-authored fixtures without a real-broker capture keep working).
//
// In shape mode the exclusions have no effect: shape mode doesn't compare
// scalar values, so there's nothing to exclude. Loaded paths are logged so
// a reviewer sees what the gate is ignoring — a silent exclusion is a
// worse failure mode than a diff.
func loadExclusions(explicit, goldenDir string, shape bool) (map[string]bool, error) {
	path := explicit
	if path == "" {
		path = filepath.Join(filepath.Dir(goldenDir), "exclusions.txt")
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && explicit == "" {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(out) == 0 {
		return out, nil
	}
	if shape {
		fmt.Fprintf(os.Stderr, "note: %d exclusion(s) in %s ignored in -shape mode\n", len(out), path)
		return map[string]bool{}, nil
	}
	fmt.Printf("exclusions loaded from %s:\n", path)
	sorted := make([]string, 0, len(out))
	for p := range out {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		fmt.Printf("  - %s\n", p)
	}
	return out, nil
}

// captureGoldens invokes the same tools the fidelity check runs, but
// writes each output to disk instead of comparing. Callers use this with
// MCP pointed at the real broker to (re)generate the ground truth.
//
// Overwrites existing files. Recapturing at the same moment as
// mock-semp/canned/capture.sh keeps live-metric drift (memory usage,
// message rates) below noise threshold in the subsequent diff.
func captureGoldens(ctx context.Context, mcpURL, broker, vpn, rdp, goldenDir string) error {
	session, err := connect(ctx, mcpURL)
	if err != nil {
		return fmt.Errorf("connecting to MCP: %w", err)
	}
	defer session.Close()

	checks := buildChecks(broker, vpn, rdp, goldenDir)

	// Two-phase write so a mid-recapture failure never leaves one golden
	// updated and another stale: first collect every tool's output and stage
	// it as a temp file next to its final path; only after every temp is on
	// disk do we rename them into place. A tool call or marshal failure
	// aborts before any rename, so the on-disk goldens are unchanged.
	type staged struct {
		tmp, final string
		size       int
	}
	var stagedFiles []staged
	cleanupTemps := func() {
		for _, s := range stagedFiles {
			_ = os.Remove(s.tmp)
		}
	}
	for _, c := range checks {
		out, err := callTool(ctx, session, c.tool, c.args)
		if err != nil {
			cleanupTemps()
			return fmt.Errorf("%s: %w", c.name(), err)
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			cleanupTemps()
			return fmt.Errorf("%s: marshaling: %w", c.name(), err)
		}
		tmp, err := stageTemp(c.goldenFile, data, 0o600)
		if err != nil {
			cleanupTemps()
			return fmt.Errorf("%s: staging %s: %w", c.name(), c.goldenFile, err)
		}
		stagedFiles = append(stagedFiles, staged{tmp: tmp, final: c.goldenFile, size: len(data)})
	}
	// All temps written; commit phase. Rename on the same directory is
	// atomic per-file on POSIX; a failure here after prior renames succeeded
	// would still leave a partial update, but at that point the filesystem
	// itself is in trouble.
	for _, s := range stagedFiles {
		if err := os.Rename(s.tmp, s.final); err != nil {
			cleanupTemps()
			return fmt.Errorf("renaming %s: %w", s.final, err)
		}
		fmt.Printf("  wrote %s (%d bytes)\n", s.final, s.size)
	}
	return nil
}

// stageTemp writes data to a fresh temp file in the same directory as the
// final path and returns the temp path. Caller is responsible for renaming
// it into place (or removing it on abort). Split from the rename step so
// captureGoldens can stage every file before committing any, keeping the
// on-disk set of goldens consistent across a partial failure.
func stageTemp(finalPath string, data []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(finalPath)
	f, err := os.CreateTemp(dir, ".golden-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func run(ctx context.Context, mcpURL, broker, vpn, rdp, goldenDir string, shape bool, exclusions map[string]bool) error {
	session, err := connect(ctx, mcpURL)
	if err != nil {
		return fmt.Errorf("connecting to MCP: %w", err)
	}
	defer session.Close()

	var firstErr error
	for _, c := range buildChecks(broker, vpn, rdp, goldenDir) {
		fmt.Printf("--- %s ---\n", c.name())
		if err := verify(ctx, session, c, shape, exclusions); err != nil {
			fmt.Printf("  DIFF: %v\n", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s diverged", c.name())
			}
			continue
		}
		fmt.Println("  match")
	}
	return firstErr
}

// toolCheck bundles one tool invocation with the golden it must match.
type toolCheck struct {
	tool       string
	goldenFile string
	args       map[string]any
	// label names the check in the output. Defaults to the tool name; set it
	// when two checks call the same tool with different arguments, so a diff
	// says which of them drifted.
	label string
}

// name returns the label to print for this check.
func (c toolCheck) name() string {
	if c.label != "" {
		return c.label
	}
	return c.tool
}

// buildChecks declares every tool the fidelity gate covers, with the exact
// arguments to call it with and the golden it must match.
//
// One list, used by both capture and verify. They used to carry a copy each,
// which meant a tool added to one and not the other would capture a golden
// nothing ever checked, or check a golden nothing ever captured.
func buildChecks(broker, vpn, rdp, goldenDir string) []toolCheck {
	return []toolCheck{
		{
			tool:       "get-broker-status",
			goldenFile: filepath.Join(goldenDir, "get-broker-status.json"),
			args:       map[string]any{"broker": broker},
		},
		{
			tool:       "list-queues",
			goldenFile: filepath.Join(goldenDir, "list-queues.json"),
			args:       map[string]any{"broker": broker, "msgVpnName": vpn},
		},
		{
			tool:       "list-rdps",
			goldenFile: filepath.Join(goldenDir, "list-rdps.json"),
			args:       map[string]any{"broker": broker, "msgVpnName": vpn},
		},
		{
			// The default maxResults of 100 stops followPages before it asks
			// for a second page, so the check above never exercises
			// pagination however many RDPs the broker holds. Asking for more
			// than one page's worth drives the mock through its cursor rule,
			// and because the golden records every RDP returned, exact-mode
			// length comparison is the assertion that all of them came back.
			tool:       "list-rdps",
			label:      "list-rdps (maxResults=200, paginated)",
			goldenFile: filepath.Join(goldenDir, "list-rdps-paged.json"),
			args:       map[string]any{"broker": broker, "msgVpnName": vpn, "maxResults": 200},
		},
		{
			tool:       "get-rdp-status",
			goldenFile: filepath.Join(goldenDir, "get-rdp-status.json"),
			args:       map[string]any{"broker": broker, "msgVpnName": vpn, "restDeliveryPointName": rdp},
		},
	}
}

// verify calls one tool, unmarshals the golden and the actual output as
// generic JSON, and returns a description of the first divergent field
// (or nil if the two match).
func verify(ctx context.Context, session *mcp.ClientSession, c toolCheck, shape bool, exclusions map[string]bool) error {
	goldenBytes, err := os.ReadFile(c.goldenFile)
	if err != nil {
		// A capture taken before this check existed leaves the manifest and
		// the golden dir agreeing with each other and simply lacking the file,
		// so the fixture preflight passes and this is the first thing to
		// notice. Without the pointer it reads as a regression in the tool.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no golden at %s — this check is newer than the capture; recapture both fixture sets with ./regen-golden.sh", c.goldenFile)
		}
		return fmt.Errorf("reading golden: %w", err)
	}
	var golden map[string]any
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		return fmt.Errorf("parsing golden JSON: %w", err)
	}

	actual, err := callTool(ctx, session, c.tool, c.args)
	if err != nil {
		return fmt.Errorf("invoking tool: %w", err)
	}

	if diff := diffJSON("", golden, actual, shape, exclusions); diff != "" {
		return errors.New(diff)
	}
	return nil
}

// callTool invokes an MCP tool and returns its structured content as a
// parsed map. MCP tool responses can carry text content; the performance
// tools emit a single JSON blob, so we take the first text part and
// decode it. This matches how the e2e agent consumes tool output.
func callTool(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) (map[string]any, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("tool returned error: %s", extractText(res.Content))
	}
	for _, part := range res.Content {
		text, ok := part.(*mcp.TextContent)
		if !ok {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
			return nil, fmt.Errorf("tool output is not JSON: %w (text=%.200s)", err, text.Text)
		}
		return out, nil
	}
	return nil, errors.New("tool returned no text content")
}

// extractText concatenates the text parts of a tool result so error
// messages are human-readable. Non-text parts are elided rather than
// printed as their Go pointer address (the default %v behaviour).
func extractText(content []mcp.Content) string {
	var out string
	for _, p := range content {
		if t, ok := p.(*mcp.TextContent); ok {
			out += t.Text
		}
	}
	if out == "" {
		return "<no text content>"
	}
	return out
}

// diffJSON returns "" if the two values are structurally equal by JSON's
// notion of equality, or a "path: golden=<x> actual=<y>" description of
// the first mismatch.
//
// Deterministic order matters: map keys are visited in sorted order and
// slices in index order, so re-running against the same inputs always
// surfaces the same first divergence — no flaky "sometimes different
// field wins" behaviour if there are multiple diffs.
//
// A path listed in exclusions (and everything under it) is skipped. The
// check fires at entry so the whole subtree short-circuits — cheaper than
// descending and filtering leaf by leaf, and it makes "exclude memory"
// mean the same thing as "exclude every descendant of memory".
func diffJSON(path string, golden, actual any, shape bool, exclusions map[string]bool) string {
	if exclusions[path] {
		return ""
	}
	switch g := golden.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: type mismatch (golden=object actual=%T)", path, actual)
		}
		keys := make([]string, 0, len(g))
		for k := range g {
			keys = append(keys, k)
		}
		for k := range a {
			if _, seen := g[k]; !seen {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, gok := g[k]
			av, aok := a[k]
			if shape {
				// Shape mode: extra keys in actual are OK (schemas grow over
				// time — additive fields must not fail the gate). But a key
				// present in golden and missing in actual IS a diff — that's
				// the mock dropping a field the real broker returns, which is
				// exactly what fidelity is meant to catch.
				if !gok {
					continue
				}
				if !aok {
					return fmt.Sprintf("%s: missing key %q in actual (golden=%v)", path, k, gv)
				}
			} else {
				switch {
				case !gok:
					return fmt.Sprintf("%s: extra key %q in actual (value=%v)", path, k, av)
				case !aok:
					return fmt.Sprintf("%s: missing key %q in actual (golden=%v)", path, k, gv)
				}
			}
			if d := diffJSON(joinPath(path, k), gv, av, shape, exclusions); d != "" {
				return d
			}
		}
		return ""

	case []any:
		a, ok := actual.([]any)
		if !ok {
			return fmt.Sprintf("%s: type mismatch (golden=array actual=%T)", path, actual)
		}
		if shape {
			// Shape mode: don't require matching length (real broker may have
			// N entries, mock may have M). Use the first golden element as the
			// template every actual element must match. Assumes the golden
			// array is homogeneous — if the SEMP response ever surfaces
			// heterogeneous elements (e.g. some slots carry `operationalState`,
			// others don't), switch to a unioned template.
			//
			// Empty-golden passes (schema may have grown additively). But if
			// golden has entries and actual is empty, that's the mock dropping
			// data the real broker returns — exactly what shape mode should
			// catch, so surface it as a diff.
			if len(g) == 0 {
				return ""
			}
			if len(a) == 0 {
				return fmt.Sprintf("%s: actual array is empty (golden has %d entries)", path, len(g))
			}
			template := g[0]
			for i, av := range a {
				if d := diffJSON(joinPath(path, "["+strconv.Itoa(i)+"]"), template, av, shape, exclusions); d != "" {
					return d
				}
			}
			return ""
		}
		if len(g) != len(a) {
			return fmt.Sprintf("%s: length mismatch (golden=%d actual=%d)", path, len(g), len(a))
		}
		for i := range g {
			if d := diffJSON(joinPath(path, "["+strconv.Itoa(i)+"]"), g[i], a[i], shape, exclusions); d != "" {
				return d
			}
		}
		return ""

	default:
		if shape {
			// Shape mode: only require that both scalars have the same JSON type.
			// json.Unmarshal into any yields nil / bool / float64 / string, so
			// this compares those four buckets and nothing else.
			if reflect.TypeOf(golden) != reflect.TypeOf(actual) {
				return fmt.Sprintf("%s: type mismatch (golden=%T actual=%T)", path, golden, actual)
			}
			return ""
		}
		if !equalScalar(golden, actual) {
			return fmt.Sprintf("%s: golden=%v (%T) actual=%v (%T)", path, golden, golden, actual, actual)
		}
		return ""
	}
}

// equalScalar compares two JSON scalars. Type is checked before value:
// json.Unmarshal into any yields nil / bool / float64 / string, and a
// value that changes bucket (number 5 becoming string "5", bool true
// becoming "true") is exactly the drift this gate exists to catch — yet
// both sides render identically under %v, so a value-only comparison
// would pass it. Values are compared as strings once the types agree,
// which keeps float formatting consistent across both sides.
func equalScalar(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	if seg != "" && seg[0] == '[' {
		return prefix + seg
	}
	return prefix + "." + seg
}

// connect opens an MCP session over the streamable HTTP transport, using
// MCP_DEV_TOKEN as a bearer if set. This mirrors test/e2e-basic-mcp/agent
// so the fidelity check works against the same MCP server configuration
// the existing e2e tests use.
func connect(ctx context.Context, url string) (*mcp.ClientSession, error) {
	httpClient := &http.Client{}
	if tok := os.Getenv("MCP_DEV_TOKEN"); tok != "" {
		httpClient.Transport = &bearer{token: tok, next: http.DefaultTransport}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fidelity", Version: "0.1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: httpClient,
	}, nil)
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(c)
}
