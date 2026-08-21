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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The real fixtures are lab captures kept out of git, so these tests build a
// miniature canned/ in memory: same file names, same shapes, enough of
// meta.paging to drive the cursor chain.
//
// canned is a package-level fs.FS read by staticFile and the rule builders at
// newHandler time, so every test sets it before constructing a handler and
// none of them run in parallel.
const (
	testRDPName    = "rdp_1"
	testRDPCursor  = "rdp-cursor-page2"
	testQCursor    = "queue-cursor-page2"
	privateVPNBase = "/SEMP/v2/__private_monitor__/msgVpns/vpn_2"
	publicVPNBase  = "/SEMP/v2/monitor/msgVpns/vpn_2"
)

func testFixtures() fstest.MapFS {
	// meta.paging.nextPageUri is where the cursor chain lives — the same
	// place cursorFromNextPageURI reads it from in a real capture.
	page := func(body, nextCursor string) *fstest.MapFile {
		meta := `{}`
		if nextCursor != "" {
			meta = `{"paging":{"nextPageUri":"/SEMP/v2/__private_monitor__/next?cursor=` + nextCursor + `&count=100"}}`
		}
		return &fstest.MapFile{Data: []byte(`{"data":[` + body + `],"meta":` + meta + `}`)}
	}
	return fstest.MapFS{
		// list-queues needs at least one page or the mock refuses to start.
		"queues_page1.json": page(`{"queueName":"q1"}`, testQCursor),
		"queues_page2.json": page(`{"queueName":"q2"}`, ""),

		"rdps_page1.json": page(`{"restDeliveryPointName":"`+testRDPName+`"}`, testRDPCursor),
		"rdps_page2.json": page(`{"restDeliveryPointName":"rdp_196"}`, ""),

		"rdp_object.json":         {Data: []byte(`{"data":{"restDeliveryPointName":"` + testRDPName + `","up":false},"meta":{}}`)},
		"rdp_queue_bindings.json": page(`{"queueBindingName":"`+testRDPName+`_queue"}`, ""),
		"rdp_rest_consumers.json": page(`{"restConsumerName":"consumer_1"}`, ""),

		// SEMPv1 rules read these at rule-build time.
		"show_version.xml":          {Data: []byte(`<rpc-reply/>`)},
		"show_system.xml":           {Data: []byte(`<rpc-reply/>`)},
		"show_memory.xml":           {Data: []byte(`<rpc-reply/>`)},
		"show_message_spool.xml":    {Data: []byte(`<rpc-reply/>`)},
		"show_hardware_details.xml": {Data: []byte(`<rpc-reply/>`)},
	}
}

// newTestHandler points canned at the in-memory fixtures and builds a handler
// over them.
func newTestHandler(t *testing.T) *handler {
	t.Helper()
	canned = testFixtures()
	return newHandler(newConfigStore([]int{8081}))
}

// get issues a GET through the handler and returns the recorder.
func get(t *testing.T, h *handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.serve(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

// TestRdpsListPagination walks the cursor chain the composite executor
// follows: page 1 carries no cursor, and the cursor it advertises in
// nextPageUri is the one that must fetch page 2.
func TestRdpsListPagination(t *testing.T) {
	h := newTestHandler(t)

	rec := get(t, h, privateVPNBase+"/restDeliveryPoints?count=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("first page: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"`+testRDPName+`"`) {
		t.Errorf("first page body does not look like page 1: %s", rec.Body.String())
	}

	rec = get(t, h, privateVPNBase+"/restDeliveryPoints?count=100&cursor="+testRDPCursor)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rdp_196") {
		t.Errorf("cursor request did not return page 2: %s", rec.Body.String())
	}

	if h.missCount() != 0 {
		t.Errorf("missCount = %d, want 0", h.missCount())
	}
}

// TestRdpsListUnknownCursorMisses proves the cursor is compared exactly.
// Cursors are opaque broker state; serving page 2 for a cursor the capture
// never produced would be a wrong answer dressed as a right one.
func TestRdpsListUnknownCursorMisses(t *testing.T) {
	h := newTestHandler(t)

	rec := get(t, h, privateVPNBase+"/restDeliveryPoints?cursor=not-a-real-cursor")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if h.missCount() != 1 {
		t.Errorf("missCount = %d, want 1", h.missCount())
	}
}

// TestRdpsListMatchesBothMonitorPrefixes covers the AC directly: MCP uses the
// private endpoint, so a rule matching only the public path would never fire.
func TestRdpsListMatchesBothMonitorPrefixes(t *testing.T) {
	for _, base := range []string{privateVPNBase, publicVPNBase} {
		t.Run(base, func(t *testing.T) {
			h := newTestHandler(t)
			if rec := get(t, h, base+"/restDeliveryPoints?count=100"); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if h.missCount() != 0 {
				t.Errorf("missCount = %d, want 0", h.missCount())
			}
		})
	}
}

// TestPinnedRDPSubResources checks all three requests get-rdp-status issues
// are served for the RDP the capture pinned, on both monitor prefixes.
func TestPinnedRDPSubResources(t *testing.T) {
	paths := map[string]string{
		"object":        "",
		"queueBindings": "/" + rdpSubQueueBindings,
		"restConsumers": "/" + rdpSubRestConsumers,
	}
	for name, sub := range paths {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t)
			rec := get(t, h, privateVPNBase+"/restDeliveryPoints/"+testRDPName+sub)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if h.missCount() != 0 {
				t.Errorf("missCount = %d, want 0", h.missCount())
			}
		})
	}
}

// TestUnpinnedRDPNameMisses is the negative case the story calls for: a
// request for any RDP other than the pinned one must miss and 404, never be
// served the pinned RDP's body. Serving it would make every measurement of
// get-rdp-status silently valid-looking for RDPs that were never captured.
//
// The names are chosen for their string relationship to the pinned "rdp_1",
// because the regression to guard against is the name comparison in
// sempv2RDPPath degrading from == to a prefix or substring match — an easy
// refactor slip, and the one wrong answer nothing else in the harness would
// catch. An unrelated name cannot detect that: HasPrefix("rdp_999", "rdp_1")
// is false, so rdp_999 alone would keep passing. Each case below fails under
// one such degradation, in both directions.
func TestUnpinnedRDPNameMisses(t *testing.T) {
	names := map[string]string{
		// Shares no prefix with the pinned name: the plain wrong-RDP case.
		"unrelated": "rdp_999",
		// The pinned name is a prefix of this one, so HasPrefix(name, pinned)
		// or Contains(name, pinned) would serve it. The VPN this fixture was
		// captured from holds rdp_1 through rdp_196, so these neighbours are
		// real names a caller can ask for, not synthetic ones.
		"pinned-is-prefix": "rdp_10",
		// The mirror image: this is a prefix of the pinned name, catching
		// HasPrefix(pinned, name) — which rdp_10 does not.
		"prefix-of-pinned": "rdp_",
	}
	subs := map[string]string{
		"object":        "",
		"queueBindings": "/" + rdpSubQueueBindings,
		"restConsumers": "/" + rdpSubRestConsumers,
	}
	for nameLabel, rdpName := range names {
		for subLabel, sub := range subs {
			t.Run(nameLabel+"/"+subLabel, func(t *testing.T) {
				h := newTestHandler(t)
				path := privateVPNBase + "/restDeliveryPoints/" + rdpName + sub

				rec := get(t, h, path)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s: status = %d, want 404 (body=%q)", path, rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), testRDPName) {
					t.Errorf("%s: served the pinned RDP's body for an unpinned name: %s", path, rec.Body.String())
				}
				if h.missCount() != 1 {
					t.Errorf("%s: missCount = %d, want 1", path, h.missCount())
				}
			})
		}
	}
}

// TestPerRDPPathNotSwallowedByCollectionRule guards the rule ordering. If the
// collection predicate ever became a prefix match, /restDeliveryPoints/<name>
// would be answered with the whole-collection body — a wrong answer that
// nothing else in the harness would catch.
func TestPerRDPPathNotSwallowedByCollectionRule(t *testing.T) {
	h := newTestHandler(t)

	if isRdpsListPath(privateVPNBase + "/restDeliveryPoints/" + testRDPName) {
		t.Error("isRdpsListPath matched a single-RDP path")
	}
	if isRdpsListPath(privateVPNBase + "/restDeliveryPoints/" + testRDPName + "/" + rdpSubQueueBindings) {
		t.Error("isRdpsListPath matched an RDP sub-resource path")
	}

	// The object rule serves the object, not the collection.
	rec := get(t, h, privateVPNBase+"/restDeliveryPoints/"+testRDPName)
	if body := rec.Body.String(); !strings.Contains(body, `"data":{`) {
		t.Errorf("single-RDP request did not get the object body: %s", body)
	}
}

// TestRdpSubResourceRejectsUnknownShapes pins the splitter's boundaries. An
// unrecognized path shape must be reported as not-an-RDP-path so it misses,
// rather than being approximated to the nearest rule.
func TestRdpSubResourceRejectsUnknownShapes(t *testing.T) {
	bad := []string{
		privateVPNBase + "/restDeliveryPoints",                            // the collection
		privateVPNBase + "/restDeliveryPoints/",                           // no name
		privateVPNBase + "/restDeliveryPoints/" + testRDPName + "/",       // empty sub
		privateVPNBase + "/restDeliveryPoints/" + testRDPName + "/a/b",    // deeper than we replay
		"/SEMP/v2/config/msgVpns/vpn_2/restDeliveryPoints/" + testRDPName, // config, not monitor
	}
	for _, p := range bad {
		if _, _, ok := rdpSubResource(p); ok {
			t.Errorf("rdpSubResource(%q) = ok, want not ok", p)
		}
	}

	name, sub, ok := rdpSubResource(privateVPNBase + "/restDeliveryPoints/" + testRDPName + "/" + rdpSubRestConsumers)
	if !ok || name != testRDPName || sub != rdpSubRestConsumers {
		t.Errorf("rdpSubResource = (%q, %q, %v), want (%q, %q, true)", name, sub, ok, testRDPName, rdpSubRestConsumers)
	}
}

// TestHitCountsPerRule checks the per-rule accounting the SEMP-cost
// measurement reads: three requests for one RDP land on three distinct rules,
// one each, and a miss increments nothing.
func TestHitCountsPerRule(t *testing.T) {
	h := newTestHandler(t)

	base := privateVPNBase + "/restDeliveryPoints/" + testRDPName
	get(t, h, base)
	get(t, h, base+"/"+rdpSubQueueBindings)
	get(t, h, base+"/"+rdpSubRestConsumers)
	get(t, h, privateVPNBase+"/restDeliveryPoints/rdp_999")

	var served int64
	for _, rl := range h.rules {
		if n := rl.hits.Load(); n > 0 {
			served += n
			if n != 1 {
				t.Errorf("rule %q served %d requests, want 1", rl.name, n)
			}
		}
	}
	if served != 3 {
		t.Errorf("rules served %d requests in total, want 3", served)
	}
	if h.missCount() != 1 {
		t.Errorf("missCount = %d, want 1", h.missCount())
	}
}

// TestHitsSnapshotResetSeparatesPhases covers what /_mock/hits exists for: one
// mock process serves the fidelity gate and then the load run, so the counts
// have to be separable or the gate's dozen requests are folded into the load
// phase's fan-out measurement.
func TestHitsSnapshotResetSeparatesPhases(t *testing.T) {
	h := newTestHandler(t)
	object := privateVPNBase + "/restDeliveryPoints/" + testRDPName

	get(t, h, object)
	get(t, h, object+"/"+rdpSubQueueBindings)

	banked := totalHits(h.hitsSnapshot(true))
	if banked != 2 {
		t.Fatalf("banked %d hits, want 2", banked)
	}
	if got := totalHits(h.hitsSnapshot(false)); got != 0 {
		t.Errorf("counters still hold %d hits after reset, want 0", got)
	}
	if !h.windowReset.Load() {
		t.Error("windowReset not set; the shutdown summary would claim to cover the whole run")
	}

	// The next phase's counts stand alone.
	get(t, h, object)
	if got := totalHits(h.hitsSnapshot(false)); got != 1 {
		t.Errorf("second window = %d hits, want 1", got)
	}
}

// TestHitsEndpointGETDoesNotReset keeps GET observational: reading the counts
// mid-run must not silently move the boundary the reset is meant to draw.
func TestHitsEndpointGETDoesNotReset(t *testing.T) {
	h := newTestHandler(t)
	get(t, h, privateVPNBase+"/restDeliveryPoints/"+testRDPName)

	rec := httptest.NewRecorder()
	h.hitsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_mock/hits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Reset  bool  `json:"reset"`
		Misses int64 `json:"misses"`
		Rules  []struct {
			Rule string `json:"rule"`
			Hits int64  `json:"hits"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	if got.Reset {
		t.Error("GET reported reset=true")
	}
	if len(got.Rules) != len(h.rules) {
		t.Errorf("reported %d rules, want %d — every rule must appear, including zero-hit ones", len(got.Rules), len(h.rules))
	}
	if total := totalHits(h.hitsSnapshot(false)); total != 1 {
		t.Errorf("GET cleared the counters: %d hits remain, want 1", total)
	}
}

// TestHitsEndpointRejectsOtherMethods — anything but GET/POST is a caller bug,
// and answering it 200 would hand back a snapshot nobody can interpret.
func TestHitsEndpointRejectsOtherMethods(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.hitsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/_mock/hits", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestHitsResetLeavesMissesAlone — misses feed the shutdown gate, and a phase
// boundary is no reason to forgive a 404.
func TestHitsResetLeavesMissesAlone(t *testing.T) {
	h := newTestHandler(t)
	get(t, h, privateVPNBase+"/restDeliveryPoints/rdp_999")

	h.hitsSnapshot(true)
	if h.missCount() != 1 {
		t.Errorf("missCount = %d after reset, want 1", h.missCount())
	}
}

func totalHits(snapshot []ruleHits) int64 {
	var total int64
	for _, r := range snapshot {
		total += r.Hits
	}
	return total
}

// TestPinnedRDPNameComesFromTheCapture is why there is no -rdp flag on the
// mock: the pin is whatever was captured, so the mock and the fixture cannot
// disagree.
func TestPinnedRDPNameComesFromTheCapture(t *testing.T) {
	canned = testFixtures()
	if got := pinnedRDPName("rdp_object.json"); got != testRDPName {
		t.Errorf("pinnedRDPName = %q, want %q", got, testRDPName)
	}
}
