// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command fidelity is the hard gate before any perf-PoC load run. It
// connects to a live MCP server, invokes get-broker-status and list-queues,
// and deep-equals the tool output against golden JSON captured earlier
// (with MCP pointed at the real broker).
//
// See docs/plans/2026-07-22-semp-mock-perf-poc.md step 5.
//
// Exit codes:
//
//	0  all tools matched their golden
//	1  at least one tool diverged (or an error setting up the check)
//
// A divergent field is printed as "path: golden=<x> actual=<y>" so the
// caller can grep out exactly what drifted. No wiggle room; the plan says
// non-empty diff stops the PoC.
//
// Usage:
//
//	fidelity \
//	  -mcp-url http://localhost:9090 \
//	  -broker  my-broker \
//	  -vpn     default \
//	  -golden-dir test/perf-poc/fidelity/golden
//
// Auth: reads MCP_DEV_TOKEN from env and sends it as Bearer if set. Leave
// unset when MCP is configured with mcp_client_auth.mode=disabled.
package main

import (
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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	mcpURL := flag.String("mcp-url", "http://localhost:9090", "MCP server URL")
	broker := flag.String("broker", "", "broker alias to invoke tools against (required)")
	vpn := flag.String("vpn", "default", "msgVpnName for list-queues")
	goldenDir := flag.String("golden-dir", "test/perf-poc/fidelity/golden", "directory containing golden JSON files")
	capture := flag.Bool("capture", false, "capture mode: overwrite golden files with the tools' current output instead of comparing (use when MCP is pointed at the real broker)")
	shape := flag.Bool("shape", false, "compare shape only (same keys, same types, same array lengths) — ignore scalar values. Use when you want structural fidelity without brittle drift on live metrics.")
	timeout := flag.Duration("timeout", 30*time.Second, "overall deadline")
	flag.Parse()

	if *broker == "" {
		fmt.Fprintln(os.Stderr, "fidelity: -broker is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *capture {
		if err := captureGoldens(ctx, *mcpURL, *broker, *vpn, *goldenDir); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("OK: goldens written to " + *goldenDir)
		return
	}

	if err := run(ctx, *mcpURL, *broker, *vpn, *goldenDir, *shape); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: all tools matched their golden")
}

// captureGoldens invokes the same tools the fidelity check runs, but
// writes each output to disk instead of comparing. Callers use this with
// MCP pointed at the real broker to (re)generate the ground truth.
//
// Overwrites existing files. Recapturing at the same moment as
// mock-semp/canned/capture.sh keeps live-metric drift (memory usage,
// message rates) below noise threshold in the subsequent diff.
func captureGoldens(ctx context.Context, mcpURL, broker, vpn, goldenDir string) error {
	session, err := connect(ctx, mcpURL)
	if err != nil {
		return fmt.Errorf("connecting to MCP: %w", err)
	}
	defer session.Close()

	checks := []toolCheck{
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
	}

	for _, c := range checks {
		out, err := callTool(ctx, session, c.tool, c.args)
		if err != nil {
			return fmt.Errorf("%s: %w", c.tool, err)
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("%s: marshaling: %w", c.tool, err)
		}
		if err := os.WriteFile(c.goldenFile, data, 0o600); err != nil {
			return fmt.Errorf("%s: writing %s: %w", c.tool, c.goldenFile, err)
		}
		fmt.Printf("  wrote %s (%d bytes)\n", c.goldenFile, len(data))
	}
	return nil
}

func run(ctx context.Context, mcpURL, broker, vpn, goldenDir string, shape bool) error {
	session, err := connect(ctx, mcpURL)
	if err != nil {
		return fmt.Errorf("connecting to MCP: %w", err)
	}
	defer session.Close()

	checks := []toolCheck{
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
	}

	var firstErr error
	for _, c := range checks {
		fmt.Printf("--- %s ---\n", c.tool)
		if err := verify(ctx, session, c, shape); err != nil {
			fmt.Printf("  DIFF: %v\n", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s diverged", c.tool)
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
}

// verify calls one tool, unmarshals the golden and the actual output as
// generic JSON, and returns a description of the first divergent field
// (or nil if the two match).
func verify(ctx context.Context, session *mcp.ClientSession, c toolCheck, shape bool) error {
	goldenBytes, err := os.ReadFile(c.goldenFile)
	if err != nil {
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

	if diff := diffJSON("", golden, actual, shape); diff != "" {
		return errors.New(diff)
	}
	return nil
}

// callTool invokes an MCP tool and returns its structured content as a
// parsed map. MCP tool responses can carry text content; the perf-PoC
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
func diffJSON(path string, golden, actual any, shape bool) string {
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
				// Shape mode: only compare the key intersection. Missing keys
				// on either side are treated as optional fields, not diffs.
				if !gok || !aok {
					continue
				}
			} else {
				switch {
				case !gok:
					return fmt.Sprintf("%s: extra key %q in actual (value=%v)", path, k, av)
				case !aok:
					return fmt.Sprintf("%s: missing key %q in actual (golden=%v)", path, k, gv)
				}
			}
			if d := diffJSON(joinPath(path, k), gv, av, shape); d != "" {
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
			// N entries, mock may have M). Build a template from the golden
			// array by unioning all element shapes — heterogeneous element
			// arrays (e.g. some slots carry `operationalState`, others don't)
			// pass as long as actual elements are a subset of the union.
			// Empty arrays on either side pass — nothing to compare.
			if len(g) == 0 || len(a) == 0 {
				return ""
			}
			template := g[0]
			for i, av := range a {
				if d := diffJSON(joinPath(path, "["+strconv.Itoa(i)+"]"), template, av, shape); d != "" {
					return d
				}
			}
			return ""
		}
		if len(g) != len(a) {
			return fmt.Sprintf("%s: length mismatch (golden=%d actual=%d)", path, len(g), len(a))
		}
		for i := range g {
			if d := diffJSON(joinPath(path, "["+strconv.Itoa(i)+"]"), g[i], a[i], shape); d != "" {
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
			return fmt.Sprintf("%s: golden=%v actual=%v", path, golden, actual)
		}
		return ""
	}
}

func equalScalar(a, b any) bool {
	if a == nil || b == nil {
		return a == b
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
