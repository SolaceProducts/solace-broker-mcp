// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// randFloat64 returns a uniformly distributed float64 in [0, 1) using
// crypto/rand. Used for error-injection sampling — not perf-critical
// because it fires at most once per mock request. On a rand.Read error
// (should be impossible on Linux) the zero buffer yields 0.0, which
// keeps the sample well-defined instead of returning NaN.
func randFloat64() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.LittleEndian.Uint64(b[:])>>11) / (1 << 53)
}

// randIntN returns a uniformly distributed int in [0, n) using crypto/rand.
// Panics if n <= 0, matching math/rand/v2.IntN. On a rand.Int error the
// fallback is 0 rather than a nil-deref panic on v.Int64() — an errored
// sample shouldn't take down the mock mid-run when injecting errors.
func randIntN(n int) int {
	if n <= 0 {
		panic("randIntN: n must be > 0")
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil || v == nil {
		return 0
	}
	return int(v.Int64())
}

// canned holds the SEMP response bodies captured from a real broker. Loaded
// from disk at startup by openCanned rather than go:embed: the captures are
// lab data, deliberately untracked, so a repo checkout has none of them and
// an embed pattern matching zero files would fail the build. Reading from
// disk also means a fresh capture takes effect on the next mock start with
// no rebuild step to forget.
var canned fs.FS

// handler routes incoming broker-port requests to a canned response based on
// a small set of hard-coded rules — one per SEMP request MCP is known to
// send. When real captures are wired in, this whole scheme swaps for a
// match-key lookup; keeping the rules explicit here means anyone reading
// the mock can see exactly what surface it covers.
type handler struct {
	cfg    *configStore
	misses atomic.Int64
	rules  []rule
}

// rule is a single "if the request looks like X, serve response Y" mapping.
// Match is intentionally loose: for SEMPv1 we substring-match the XML body
// (SEMP requests are small enough that a substring is unambiguous). For
// SEMPv2 we path-prefix + query-param match.
type rule struct {
	name    string
	match   func(r *http.Request, body []byte) bool
	respond func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newHandler(cfg *configStore) *handler {
	h := &handler{cfg: cfg}
	h.rules = h.buildRules()
	return h
}

// withInjection wraps the handler with per-port latency and error overrides
// from the config store. Config is read atomically on every request so
// updates via /_mock/config apply on the next call, no restart needed.
func (h *handler) withInjection(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override := h.cfg.get(port)
		if override.latencyMs > 0 {
			time.Sleep(time.Duration(override.latencyMs) * time.Millisecond)
		}
		if override.errorRate > 0 && randFloat64() < override.errorRate {
			// Enforce the total-error budget: decrement first, and only
			// inject when the pre-decrement value was positive. Using
			// Add(-1) >= 0 keeps concurrent callers honest without a lock.
			if override.errorBudget != nil && override.errorBudget.Add(-1) < 0 {
				// budget exhausted — fall through to the canned response
			} else {
				code := pickErrorStatus(override.errorStatuses)
				http.Error(w, "injected error", code)
				return
			}
		}
		h.serve(w, r)
	})
}

func (h *handler) missCount() int64 { return h.misses.Load() }

// serve reads the body once (rules need it), then dispatches to the first
// matching rule. On miss, logs the request line and returns 404.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	for _, rl := range h.rules {
		if rl.match(r, body) {
			rl.respond(w, r, body)
			return
		}
	}

	h.misses.Add(1)
	log.Printf("mock-semp MISS: %s %s%s body=%q",
		r.Method, r.URL.Path, queryFor(r), truncate(body, 200))
	http.NotFound(w, r)
}

// buildRules declares the exact set of SEMP requests this mock knows how
// to answer. Ordering matters: first match wins, so more specific rules
// (e.g. queues pagination cursor) come before broader ones.
//
// list-queues pagination auto-discovers pages: any canned/queues_page<N>.json
// present on disk becomes a rule keyed on the cursor value MCP will
// send to reach that page (extracted from page N-1's meta.paging.nextPageUri).
// So a fresh capture with more pages Just Works — no matching handler edit
// needed. A capture with only page 1 (queue count <= page size) works too:
// no cursor rules registered, just the page-1 rule.
func (h *handler) buildRules() []rule {
	rules := []rule{
		// SEMPv1 — get-broker-status issues four <show> commands in
		// parallel. Order here is stylistic; SEMPv1 dispatch is body-based.
		{
			name:    "sempv1 show version",
			match:   sempv1Body("<version"),
			respond: staticFile("show_version.xml", "application/xml"),
		},
		{
			name:    "sempv1 show system",
			match:   sempv1Body("<system"),
			respond: staticFile("show_system.xml", "application/xml"),
		},
		{
			name:    "sempv1 show memory",
			match:   sempv1Body("<memory"),
			respond: staticFile("show_memory.xml", "application/xml"),
		},
		{
			name:    "sempv1 show message-spool",
			match:   sempv1Body("<message-spool"),
			respond: staticFile("show_message_spool.xml", "application/xml"),
		},
		// Appliance-only: fires when the show-version description identifies
		// the broker as an appliance (see brokerstatus/handler.go). Rule
		// registered unconditionally; if the broker's show_version says
		// software/VMR, MCP simply won't send this request.
		{
			name:    "sempv1 show hardware details",
			match:   sempv1Body("<hardware"),
			respond: staticFile("show_hardware_details.xml", "application/xml"),
		},
	}

	rules = append(rules, buildQueuesRules()...)
	return rules
}

// buildQueuesRules discovers canned/queues_page<N>.json files and returns
// one rule per page. Pages 2..N are matched by the cursor MCP will send,
// which is the cursor embedded in page N-1's nextPageUri (MCP passes the
// URI back verbatim). Cursor rules are registered before the page-1 rule
// so a cursor-bearing request is matched specifically.
func buildQueuesRules() []rule {
	entries, err := fs.ReadDir(canned, ".")
	if err != nil {
		log.Fatalf("mock-semp: reading canned dir: %v", err)
	}
	type page struct {
		num  int
		file string
	}
	var pages []page
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "queues_page") || !strings.HasSuffix(n, ".json") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(n, "queues_page"), ".json")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			log.Fatalf("mock-semp: canned/%s: cannot parse page number: %v", n, err)
		}
		pages = append(pages, page{num: num, file: n})
	}
	if len(pages) == 0 {
		log.Fatalf("mock-semp: no queues_page*.json in the canned dir — list-queues cannot be served; capture fixtures with ./regen-golden.sh")
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].num < pages[j].num })
	// Require contiguous 1..N. A gap means the recapture failed partway
	// through (or someone hand-edited canned/) and the pagination chain
	// would break silently at runtime — fail fast at startup instead.
	for i, p := range pages {
		if p.num != i+1 {
			log.Fatalf("mock-semp: canned/queues_page*.json not contiguous: expected page %d, got %d", i+1, p.num)
		}
	}

	var out []rule
	// Pages 2..N — cursor match. The cursor MCP sends for page N is the one
	// baked into page (N-1)'s nextPageUri.
	for i := 1; i < len(pages); i++ {
		cursor := cursorFromNextPageURI(pages[i-1].file)
		if cursor == "" {
			log.Fatalf("mock-semp: %s has no nextPageUri cursor but %s exists", pages[i-1].file, pages[i].file)
		}
		out = append(out, rule{
			name:    fmt.Sprintf("sempv2 queues page %d (cursor)", pages[i].num),
			match:   sempv2QueuesCursorEquals(cursor),
			respond: staticFile(pages[i].file, "application/json"),
		})
	}
	// Page 1 — no cursor.
	out = append(out, rule{
		name:    "sempv2 queues page 1",
		match:   sempv2QueuesFirstPage,
		respond: staticFile(pages[0].file, "application/json"),
	})
	return out
}

// cursorFromNextPageURI returns the cursor query parameter carried in the
// given canned page's meta.paging.nextPageUri, or "" if the page has no
// nextPageUri (i.e. it is the last page). Fatals on a malformed page: a
// broken canned file is a bad capture, caught at startup rather than
// mid-run.
func cursorFromNextPageURI(name string) string {
	data, err := fs.ReadFile(canned, name)
	if err != nil {
		log.Fatalf("mock-semp: reading %s: %v", name, err)
	}
	var parsed struct {
		Meta struct {
			Paging struct {
				NextPageURI string `json:"nextPageUri"`
			} `json:"paging"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Fatalf("mock-semp: %s: bad JSON: %v", name, err)
	}
	if parsed.Meta.Paging.NextPageURI == "" {
		return ""
	}
	u, err := url.Parse(parsed.Meta.Paging.NextPageURI)
	if err != nil {
		log.Fatalf("mock-semp: %s: bad nextPageUri: %v", name, err)
	}
	return u.Query().Get("cursor")
}

// sempv1Body returns a match predicate that fires when the request is a
// POST to /SEMP whose body contains the given substring. SEMP payloads
// don't share tag names across commands, so substring matching is
// unambiguous for the four <show> commands get-broker-status issues.
func sempv1Body(needle string) func(*http.Request, []byte) bool {
	needleBytes := []byte(needle)
	return func(r *http.Request, body []byte) bool {
		return r.Method == http.MethodPost &&
			r.URL.Path == "/SEMP" &&
			bytes.Contains(body, needleBytes)
	}
}

// isQueuesListPath matches both the public monitor path and the private
// monitor path. MCP uses the private endpoint for list-queues because it
// exposes bindCount (a private-schema attribute); direct curl against the
// public path silently drops it. Both paths are shape-compatible for the
// fields we replay, so a single rule serves both.
func isQueuesListPath(p string) bool {
	if !strings.HasSuffix(p, "/queues") {
		return false
	}
	return strings.HasPrefix(p, "/SEMP/v2/monitor/msgVpns/") ||
		strings.HasPrefix(p, "/SEMP/v2/__private_monitor__/msgVpns/")
}

func sempv2QueuesFirstPage(r *http.Request, _ []byte) bool {
	return r.Method == http.MethodGet &&
		isQueuesListPath(r.URL.Path) &&
		r.URL.Query().Get("cursor") == ""
}

// sempv2QueuesCursorEquals returns a predicate that fires when the request
// is a list-queues GET whose cursor query parameter matches exactly. Cursor
// values are opaque broker state (URL-encoded XML), so exact match is the
// only correct comparison.
func sempv2QueuesCursorEquals(want string) func(*http.Request, []byte) bool {
	return func(r *http.Request, _ []byte) bool {
		return r.Method == http.MethodGet &&
			isQueuesListPath(r.URL.Path) &&
			r.URL.Query().Get("cursor") == want
	}
}

// staticFile returns a responder that serves the given canned file with the
// given content-type. The file is read once at startup and a read error is
// fatal there — a fixture that's missing or unreadable means the capture is
// incomplete, and failing at startup beats 404ing mid-run.
func staticFile(name, contentType string) func(http.ResponseWriter, *http.Request, []byte) {
	data, err := fs.ReadFile(canned, name)
	if err != nil {
		log.Fatalf("mock-semp: missing canned file %q: %v; capture fixtures with ./regen-golden.sh", name, err)
	}
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

// pickErrorStatus returns a status code from the weighted pool, defaulting
// to 500 when the pool is empty (preserves original behavior when the
// caller sets error_rate but no error_statuses).
func pickErrorStatus(pool []weightedStatus) int {
	if len(pool) == 0 {
		return http.StatusInternalServerError
	}
	total := 0
	for _, s := range pool {
		total += s.weight
	}
	pick := randIntN(total)
	for _, s := range pool {
		if pick < s.weight {
			return s.code
		}
		pick -= s.weight
	}
	return pool[len(pool)-1].code // unreachable modulo integer overflow
}

func queryFor(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
