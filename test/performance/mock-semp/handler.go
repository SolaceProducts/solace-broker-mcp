// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/binary"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// randFloat64 returns a uniformly distributed float64 in [0, 1) using
// crypto/rand. Used for error-injection sampling — not perf-critical
// because it fires at most once per mock request.
func randFloat64() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.LittleEndian.Uint64(b[:])>>11) / (1 << 53)
}

// randIntN returns a uniformly distributed int in [0, n) using crypto/rand.
// Panics if n <= 0, matching math/rand/v2.IntN.
func randIntN(n int) int {
	if n <= 0 {
		panic("randIntN: n must be > 0")
	}
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// canned holds the hand-authored SEMP response bodies embedded at build
// time. Real captures will replace these once lab access is available.
//
//go:embed canned/*
var canned embed.FS

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
// Page-2 for list-queues is only registered if canned/queues_page2.json
// exists in the embed. Small broker captures (queue count <= page size)
// produce only page 1; forcing a page-2 file in that case would either
// leave stale placeholder data lying around or fail startup. Bigger
// captures re-enable the rule automatically.
func (h *handler) buildRules() []rule {
	rules := []rule{
		// SEMPv1 — get-broker-status issues four <show> commands in
		// parallel. Order here is stylistic; SEMPv1 dispatch is body-based.
		{
			name:    "sempv1 show version",
			match:   sempv1Body("<version"),
			respond: staticFile("canned/show_version.xml", "application/xml"),
		},
		{
			name:    "sempv1 show system",
			match:   sempv1Body("<system"),
			respond: staticFile("canned/show_system.xml", "application/xml"),
		},
		{
			name:    "sempv1 show memory",
			match:   sempv1Body("<memory"),
			respond: staticFile("canned/show_memory.xml", "application/xml"),
		},
		{
			name:    "sempv1 show message-spool",
			match:   sempv1Body("<message-spool"),
			respond: staticFile("canned/show_message_spool.xml", "application/xml"),
		},
		// Appliance-only: fires when the show-version description identifies
		// the broker as an appliance (see brokerstatus/handler.go). Rule
		// registered unconditionally; if the broker's show_version says
		// software/VMR, MCP simply won't send this request.
		{
			name:    "sempv1 show hardware details",
			match:   sempv1Body("<hardware"),
			respond: staticFile("canned/show_hardware_details.xml", "application/xml"),
		},
	}

	// SEMPv2 queues page 2 — optional. Registered BEFORE page 1 so a
	// cursor-bearing request is matched here first (page 1's predicate
	// checks the same path prefix).
	if _, err := canned.ReadFile("canned/queues_page2.json"); err == nil {
		rules = append(rules, rule{
			name:    "sempv2 queues page 2 (cursor)",
			match:   sempv2QueuesWithCursor,
			respond: staticFile("canned/queues_page2.json", "application/json"),
		})
	}
	rules = append(rules, rule{
		name:    "sempv2 queues page 1",
		match:   sempv2QueuesFirstPage,
		respond: staticFile("canned/queues_page1.json", "application/json"),
	})
	return rules
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

func sempv2QueuesWithCursor(r *http.Request, _ []byte) bool {
	return r.Method == http.MethodGet &&
		isQueuesListPath(r.URL.Path) &&
		r.URL.Query().Get("cursor") != ""
}

// staticFile returns a responder that serves the given embedded file with
// the given content-type. Errors reading the embedded fs are fatal at
// startup — canned is baked into the binary, so a miss here is a build
// mistake, not runtime.
func staticFile(path, contentType string) func(http.ResponseWriter, *http.Request, []byte) {
	data, err := canned.ReadFile(path)
	if err != nil {
		log.Fatalf("mock-semp: missing canned file %q: %v", path, err)
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
