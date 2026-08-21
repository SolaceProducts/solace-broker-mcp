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
	rules  []*rule
	// windowReset records that the counters were zeroed mid-run via
	// POST /_mock/hits, so the shutdown summary can say which window its
	// numbers cover instead of implying they cover the whole process.
	windowReset atomic.Bool
}

// rule is a single "if the request looks like X, serve response Y" mapping.
// Matching is as tight as the protocol allows, and no tighter: SEMPv1
// substring-matches the XML body because all five <show> commands POST to the
// same /SEMP path, while SEMPv2 matches on path and query parameters, which
// are unambiguous. Held by pointer so hits can be counted without copying an
// atomic.
type rule struct {
	name    string
	match   func(r *http.Request, body []byte) bool
	respond func(w http.ResponseWriter, r *http.Request, body []byte)
	hits    atomic.Int64
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

// logHitSummary prints how many requests each rule served, in registration
// order. This is the harness's own measurement of SEMP fan-out: a known number
// of tool calls reports exactly how many SEMP requests they cost, which is
// what converts loadgen's tool-calls/s into the SEMP requests/s the
// performance targets are written in.
//
// The window matters and is stated in the header. One mock process serves both
// the fidelity gate and the load run, so without a mid-run reset these totals
// are the sum of the two — a fixed dozen requests from the gate, which is
// nothing next to a load run and everything next to a calibration one. The run
// scripts POST /_mock/hits after the gate to bank its counts and zero the
// counters, and this says so rather than leaving the reader to know it.
//
// Zero-hit rules are called out because a rule that never fires is a real
// hazard — a predicate matching only the public monitor path would look
// perfectly healthy otherwise. It is reported, never gating: a run with
// -tools list-rdps legitimately never touches the queues or SEMPv1 rules.
func (h *handler) logHitSummary() {
	window := "this run"
	if h.windowReset.Load() {
		window = "since the last POST /_mock/hits reset"
	}
	log.Printf("mock-semp: SEMP requests served per rule (%s):", window)
	for _, rl := range h.rules {
		n := rl.hits.Load()
		note := ""
		if n == 0 {
			note = "   (never fired)"
		}
		log.Printf("mock-semp:   %-44s %8d%s", rl.name, n, note)
	}
}

// ruleHits is one rule's served-request count, as reported by /_mock/hits.
type ruleHits struct {
	Rule string `json:"rule"`
	Hits int64  `json:"hits"`
}

// hitsSnapshot returns every rule's count in registration order. When reset is
// true the counters are zeroed as they are read, so the returned snapshot is
// the tally for the window that just ended and the next window starts at zero.
//
// Swap-per-rule is not one atomic operation across all rules, which is fine
// for the only caller: the scripts snapshot between phases, when nothing is
// driving the mock. A snapshot taken under load may split a request or two
// across the boundary.
func (h *handler) hitsSnapshot(reset bool) []ruleHits {
	out := make([]ruleHits, 0, len(h.rules))
	for _, rl := range h.rules {
		n := rl.hits.Load()
		if reset {
			n = rl.hits.Swap(0)
		}
		out = append(out, ruleHits{Rule: rl.name, Hits: n})
	}
	if reset {
		h.windowReset.Store(true)
	}
	return out
}

// hitsHandler serves /_mock/hits on the config port. GET reports the per-rule
// counts; POST reports them and zeroes the counters, which is how a caller
// separates one phase's SEMP fan-out from the next's. It lives on the config
// port rather than the broker ports for the same reason /_mock/config does:
// broker ports may be open to the LAN, and a path under /_mock there would
// also be one more predicate every real SEMP request has to fall through.
//
// The miss count is reported for context but never reset: it feeds the
// shutdown gate, and a phase boundary is no reason to forgive a 404.
func (h *handler) hitsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodPost:
		default:
			http.Error(w, "GET (report) or POST (report and reset) only", http.StatusMethodNotAllowed)
			return
		}
		snapshot := h.hitsSnapshot(r.Method == http.MethodPost)
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Reset  bool       `json:"reset"`
			Misses int64      `json:"misses"`
			Rules  []ruleHits `json:"rules"`
		}{
			Reset:  r.Method == http.MethodPost,
			Misses: h.missCount(),
			Rules:  snapshot,
		})
	})
}

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
			rl.hits.Add(1)
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
// (e.g. a pagination cursor, or one pinned RDP) come before broader ones.
//
// List pagination auto-discovers pages: any canned/<prefix>_page<N>.json
// present on disk becomes a rule keyed on the cursor value MCP will
// send to reach that page (extracted from page N-1's meta.paging.nextPageUri).
// So a fresh capture with more pages Just Works — no matching handler edit
// needed. A capture with only page 1 (object count <= page size) works too:
// no cursor rules registered, just the page-1 rule.
func (h *handler) buildRules() []*rule {
	rules := []*rule{
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

	// Per-RDP rules precede the restDeliveryPoints collection rules. Suffix
	// matching already keeps /restDeliveryPoints/<name> out of the collection
	// rule, but the ordering makes that structural rather than a property of
	// how the predicates happen to be written today.
	rules = append(rules, buildPinnedRDPRules()...)
	rules = append(rules, buildPagedRules(queuesCollection)...)
	rules = append(rules, buildPagedRules(rdpsCollection)...)
	return rules
}

// pagedCollection describes one paginated SEMPv2 collection the mock
// replays. Both collections behave identically — page files on disk, a cursor
// chain between them — so they share one builder rather than each carrying a
// copy of the discovery logic.
type pagedCollection struct {
	label      string            // appears in rule names and startup errors
	filePrefix string            // canned/<filePrefix><N>.json
	tool       string            // the MCP tool that cannot be served without it
	isListPath func(string) bool // does this request path address the collection?
	// ownsPaginationCheck marks the collection the fidelity gate's paginated
	// check runs against. Exactly one collection carries it, and a capture
	// that yields a single page for that one leaves the cursor chain
	// unexercised suite-wide — see the warning in buildPagedRules.
	ownsPaginationCheck bool
}

var (
	queuesCollection = pagedCollection{
		label:      "queues",
		filePrefix: "queues_page",
		tool:       "list-queues",
		isListPath: isQueuesListPath,
	}
	rdpsCollection = pagedCollection{
		label:               "rdps",
		filePrefix:          "rdps_page",
		tool:                "list-rdps",
		isListPath:          isRdpsListPath,
		ownsPaginationCheck: true,
	}
)

// buildPagedRules discovers canned/<prefix><N>.json files and returns one
// rule per page. Pages 2..N are matched by the cursor MCP will send, which is
// the cursor embedded in page N-1's nextPageUri (MCP passes the URI back
// verbatim). Cursor rules are registered before the page-1 rule so a
// cursor-bearing request is matched specifically.
func buildPagedRules(c pagedCollection) []*rule {
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
		if e.IsDir() || !strings.HasPrefix(n, c.filePrefix) || !strings.HasSuffix(n, ".json") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(n, c.filePrefix), ".json")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			log.Fatalf("mock-semp: canned/%s: cannot parse page number: %v", n, err)
		}
		pages = append(pages, page{num: num, file: n})
	}
	if len(pages) == 0 {
		log.Fatalf("mock-semp: no %s*.json in the canned dir — %s cannot be served; capture fixtures with ./regen-golden.sh",
			c.filePrefix, c.tool)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].num < pages[j].num })
	// Require contiguous 1..N. A gap means the recapture failed partway
	// through (or someone hand-edited canned/) and the pagination chain
	// would break silently at runtime — fail fast at startup instead.
	for i, p := range pages {
		if p.num != i+1 {
			log.Fatalf("mock-semp: canned/%s*.json not contiguous: expected page %d, got %d", c.filePrefix, i+1, p.num)
		}
	}

	// A single page means no cursor rule is registered, so nothing in the
	// suite exercises the pagination chain — and the fidelity check that
	// exists to cover it still passes, because it compares a one-page golden
	// against a one-page replay. Green, and covering less than it says. The
	// capture is the only place to fix it, so say so loudly at startup rather
	// than leaving it to be inferred from the hit summary.
	if len(pages) == 1 && c.ownsPaginationCheck {
		log.Printf("mock-semp: WARNING: canned/%s1.json is the only %s page, so no cursor rules exist. "+
			"The paginated %s fidelity check cannot exercise pagination against this capture — "+
			"recapture from a VPN holding more than one page (>100 objects) to restore that coverage.",
			c.filePrefix, c.label, c.tool)
	}

	var out []*rule
	// Pages 2..N — cursor match. The cursor MCP sends for page N is the one
	// baked into page (N-1)'s nextPageUri.
	for i := 1; i < len(pages); i++ {
		cursor := cursorFromNextPageURI(pages[i-1].file)
		if cursor == "" {
			log.Fatalf("mock-semp: %s has no nextPageUri cursor but %s exists", pages[i-1].file, pages[i].file)
		}
		out = append(out, &rule{
			name:    fmt.Sprintf("sempv2 %s page %d (cursor)", c.label, pages[i].num),
			match:   sempv2ListCursorEquals(c.isListPath, cursor),
			respond: staticFile(pages[i].file, "application/json"),
		})
	}
	// Page 1 — no cursor.
	out = append(out, &rule{
		name:    fmt.Sprintf("sempv2 %s page 1", c.label),
		match:   sempv2ListFirstPage(c.isListPath),
		respond: staticFile(pages[0].file, "application/json"),
	})
	return out
}

// Sub-resources of a single RDP that get-rdp-status reads. Named constants
// because the strings appear in both the rule set and its tests.
const (
	rdpSubQueueBindings = "queueBindings"
	rdpSubRestConsumers = "restConsumers"
)

// buildPinnedRDPRules returns the three exact-path rules behind
// get-rdp-status: the RDP object, its queue bindings, and its REST consumers.
//
// The RDP name is taken from the captured object itself, not from a flag or a
// literal, so the mock's pin cannot disagree with what was captured. Every
// other name matches no rule at all and lands in the MISS log with a 404 —
// deliberately unlike the five SEMPv1 rules, which share the /SEMP path and
// are told apart by first-substring-match on the request body. That shape
// answers a request for the wrong thing with a plausible body; this one
// fails loudly.
//
// One page per sub-resource is enough while the pinned RDP has a single queue
// binding and a single REST consumer (get-rdp-status asks for count=100).
// A pinned RDP with more than 100 of either would need page files here, and
// would announce itself as a miss rather than a truncated answer.
func buildPinnedRDPRules() []*rule {
	const objectFile = "rdp_object.json"
	pinned := pinnedRDPName(objectFile)

	return []*rule{
		{
			name:    fmt.Sprintf("sempv2 rdp %q object", pinned),
			match:   sempv2RDPPath(pinned, ""),
			respond: staticFile(objectFile, "application/json"),
		},
		{
			name:    fmt.Sprintf("sempv2 rdp %q queueBindings", pinned),
			match:   sempv2RDPPath(pinned, rdpSubQueueBindings),
			respond: staticFile("rdp_queue_bindings.json", "application/json"),
		},
		{
			name:    fmt.Sprintf("sempv2 rdp %q restConsumers", pinned),
			match:   sempv2RDPPath(pinned, rdpSubRestConsumers),
			respond: staticFile("rdp_rest_consumers.json", "application/json"),
		},
	}
}

// pinnedRDPName returns the restDeliveryPointName carried in the captured RDP
// object. Fatals when the file is missing, unparseable, or carries no name:
// a mock that pinned "" would answer for an RDP nobody asked about, and a bad
// capture is a startup problem, not a runtime one.
func pinnedRDPName(file string) string {
	data, err := fs.ReadFile(canned, file)
	if err != nil {
		log.Fatalf("mock-semp: reading %s: %v — get-rdp-status cannot be served; capture fixtures with ./regen-golden.sh", file, err)
	}
	var parsed struct {
		Data struct {
			RestDeliveryPointName string `json:"restDeliveryPointName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Fatalf("mock-semp: %s: bad JSON: %v", file, err)
	}
	if parsed.Data.RestDeliveryPointName == "" {
		log.Fatalf("mock-semp: %s: no data.restDeliveryPointName — recapture with ./regen-golden.sh", file)
	}
	return parsed.Data.RestDeliveryPointName
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

// isMonitorMsgVpnPath reports whether p addresses something under a message
// VPN on either monitor endpoint. MCP uses the private one in practice — the
// monitor spec's basePath is /SEMP/v2/__private_monitor__, and list-queues
// needs it for bindCount, a private-schema attribute the public path rejects.
// A rule matching only the public path would therefore never fire, so every
// SEMPv2 predicate here goes through this function rather than naming one
// prefix. Both paths are shape-compatible for the fields we replay.
func isMonitorMsgVpnPath(p string) bool {
	return strings.HasPrefix(p, "/SEMP/v2/monitor/msgVpns/") ||
		strings.HasPrefix(p, "/SEMP/v2/__private_monitor__/msgVpns/")
}

func isQueuesListPath(p string) bool {
	return isMonitorMsgVpnPath(p) && strings.HasSuffix(p, "/queues")
}

// isRdpsListPath matches the restDeliveryPoints collection. Suffix-matching
// (not prefix) is what keeps /restDeliveryPoints/<name> and its sub-resources
// out of this rule — those belong to the pinned-RDP rules.
func isRdpsListPath(p string) bool {
	return isMonitorMsgVpnPath(p) && strings.HasSuffix(p, "/restDeliveryPoints")
}

// sempv2ListFirstPage returns a predicate for the cursor-free first page of a
// collection.
func sempv2ListFirstPage(isListPath func(string) bool) func(*http.Request, []byte) bool {
	return func(r *http.Request, _ []byte) bool {
		return r.Method == http.MethodGet &&
			isListPath(r.URL.Path) &&
			r.URL.Query().Get("cursor") == ""
	}
}

// sempv2ListCursorEquals returns a predicate that fires when the request is a
// GET for the collection whose cursor query parameter matches exactly. Cursor
// values are opaque broker state (URL-encoded XML), so exact match is the
// only correct comparison.
func sempv2ListCursorEquals(isListPath func(string) bool, want string) func(*http.Request, []byte) bool {
	return func(r *http.Request, _ []byte) bool {
		return r.Method == http.MethodGet &&
			isListPath(r.URL.Path) &&
			r.URL.Query().Get("cursor") == want
	}
}

// rdpsSegment is the path element that introduces a single RDP.
const rdpsSegment = "/restDeliveryPoints/"

// rdpSubResource splits a monitor path of the form
//
//	<monitor-prefix>/msgVpns/<vpn>/restDeliveryPoints/<name>[/<sub>]
//
// into the RDP name and its sub-resource ("" for the object itself). ok is
// false for anything that is not such a path, including a deeper path than
// this mock replays — an unrecognized shape must miss, not be approximated.
//
// The VPN segment is deliberately not compared: the mock replays one capture
// on every port regardless of the alias or VPN in the request, exactly as the
// queues rules do. The RDP name is what gets pinned.
func rdpSubResource(p string) (name, sub string, ok bool) {
	if !isMonitorMsgVpnPath(p) {
		return "", "", false
	}
	_, tail, found := strings.Cut(p, rdpsSegment)
	if !found {
		return "", "", false
	}
	name, sub, hasSub := strings.Cut(tail, "/")
	switch {
	case name == "":
		return "", "", false
	case !hasSub:
		return name, "", true
	case sub == "" || strings.Contains(sub, "/"):
		return "", "", false
	default:
		return name, sub, true
	}
}

// sempv2RDPPath returns a predicate matching a GET for exactly one RDP name
// and one sub-resource. A request naming any other RDP matches nothing and is
// answered as a miss.
func sempv2RDPPath(pinned, sub string) func(*http.Request, []byte) bool {
	return func(r *http.Request, _ []byte) bool {
		if r.Method != http.MethodGet {
			return false
		}
		name, gotSub, ok := rdpSubResource(r.URL.Path)
		return ok && name == pinned && gotSub == sub
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
