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

package composite_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess/postprocesstest"
)

func TestApplyResultStrategy_Collect(t *testing.T) {
	stepResults := map[string]map[string]any{
		"a": {"x": 1},
		"b": {"y": 2},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": map[string]any{"x": 1},
		"b": map[string]any{"y": 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestApplyResultStrategy_PostProcess_MergesSummary(t *testing.T) {
	postprocesstest.Register(t, "__test_summary", postprocess.Handler{
		Fn: func(map[string]map[string]any) (map[string]any, error) {
			return map[string]any{"count": 7}, nil
		},
	})
	stepResults := map[string]map[string]any{
		"queues": {"data": []any{}},
	}
	got, err := composite.ApplyResultStrategy(
		composite.ResultStrategy{Strategy: "postProcess", PostProcess: "__test_summary"},
		stepResults,
	)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := got["summary"].(map[string]any)
	if !ok || summary["count"] != 7 {
		t.Fatalf("summary: got %+v", got["summary"])
	}
	queues, ok := got["queues"].(map[string]any)
	if !ok || !reflect.DeepEqual(queues["data"], []any{}) {
		t.Fatalf("queues: got %+v", got["queues"])
	}
}

func TestApplyResultStrategy_PostProcess_OmitRawSteps(t *testing.T) {
	postprocesstest.Register(t, "__test_summary_omit", postprocess.Handler{
		Fn: func(map[string]map[string]any) (map[string]any, error) {
			return map[string]any{"count": 7}, nil
		},
	})
	stepResults := map[string]map[string]any{
		"queues": {"data": []any{"item1", "item2"}},
	}
	got, err := composite.ApplyResultStrategy(
		composite.ResultStrategy{Strategy: "postProcess", PostProcess: "__test_summary_omit", OmitRawSteps: true},
		stepResults,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"summary": map[string]any{"count": 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v (raw step %q must not survive when OmitRawSteps is set)", got, want, "queues")
	}
}

func TestApplyResultStrategy_Unsupported(t *testing.T) {
	_, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "bogus"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
}

// SOL-155432: the broker's response envelope ({"data":..., "meta":{...},
// "links":{...}}) must never reach the caller unstripped — links.uri,
// links.subscriptionsUri, meta.request.uri, and meta.paging.nextPageUri all
// leak the broker's management hostname:port, SEMP API variant, VPN name,
// and resource path through ordinary successful tool use.

func TestApplyResultStrategy_Collect_StripsLinksAndMetaRequestURI(t *testing.T) {
	stepResults := map[string]map[string]any{
		"createQueue": {
			"data": map[string]any{"queueName": "orders", "accessType": "exclusive"},
			"meta": map[string]any{
				"request":      map[string]any{"method": "POST", "uri": "https://broker:943/SEMP/v2/__private_config__/msgVpns/default/queues"},
				"responseCode": float64(200),
			},
			"links": map[string]any{
				"uri":              "https://broker:943/SEMP/v2/__private_config__/msgVpns/default/queues/orders",
				"subscriptionsUri": "https://broker:943/SEMP/v2/__private_config__/msgVpns/default/queues/orders/subscriptions",
			},
		},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	step := got["createQueue"].(map[string]any)
	if _, present := step["links"]; present {
		t.Errorf("links must not survive: got %+v", step["links"])
	}
	meta := step["meta"].(map[string]any)
	request, ok := meta["request"].(map[string]any)
	if !ok {
		t.Fatalf("meta.request: got %T, want map[string]any", meta["request"])
	}
	if _, present := request["uri"]; present {
		t.Errorf("meta.request.uri must not survive: got %+v", request)
	}
	// method is not uri-bearing and carries no broker identity — kept,
	// alongside the sibling request map itself (only the leaf is stripped).
	if request["method"] != "POST" {
		t.Errorf("meta.request.method should survive untouched: got %+v", request)
	}
	if meta["responseCode"] != float64(200) {
		t.Errorf("meta.responseCode should survive untouched: got %+v", meta["responseCode"])
	}
	// data is the resource's own attributes — must survive completely untouched.
	data := step["data"].(map[string]any)
	want := map[string]any{"queueName": "orders", "accessType": "exclusive"}
	if !reflect.DeepEqual(data, want) {
		t.Errorf("data must survive untouched: got %+v, want %+v", data, want)
	}
}

func TestApplyResultStrategy_StripsNestedPagingNextPageURI(t *testing.T) {
	// meta.paging.nextPageUri sits one level deeper than meta.request.uri —
	// a flat (non-recursive) strip of meta's own top-level keys would miss it.
	stepResults := map[string]map[string]any{
		"probe": {
			"data": []any{},
			"meta": map[string]any{
				"paging":       map[string]any{"nextPageUri": "https://broker:943/SEMP/v2/monitor/msgVpns/default/clients?cursor=abc"},
				"responseCode": float64(200),
			},
		},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	meta := got["probe"].(map[string]any)["meta"].(map[string]any)
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		t.Fatalf("meta.paging: got %T, want map[string]any", meta["paging"])
	}
	if _, present := paging["nextPageUri"]; present {
		t.Errorf("meta.paging.nextPageUri must not survive: got %+v", paging)
	}
	if meta["responseCode"] != float64(200) {
		t.Errorf("meta.responseCode should survive untouched: got %+v", meta["responseCode"])
	}
}

func TestApplyResultStrategy_StripsByKeyFanOutEnvelopes(t *testing.T) {
	// A future fan-out tool without list_vpns.go's own custom sanitization
	// would otherwise ship each row's raw envelope verbatim.
	stepResults := map[string]map[string]any{
		"real-clients": {
			"byKey": map[string]any{
				"vpn-a": map[string]any{
					"data":  []any{},
					"links": map[string]any{"uri": "https://broker:943/SEMP/v2/monitor/msgVpns/vpn-a/clients"},
					"meta": map[string]any{"request": map[string]any{
						"method": "GET",
						"uri":    "https://broker:943/SEMP/v2/monitor/msgVpns/vpn-a/clients",
					}},
				},
			},
		},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	byKey := got["real-clients"].(map[string]any)["byKey"].(map[string]any)
	entry := byKey["vpn-a"].(map[string]any)
	if _, present := entry["links"]; present {
		t.Errorf("byKey entry's links must not survive: got %+v", entry["links"])
	}
	meta, ok := entry["meta"].(map[string]any)
	if !ok {
		t.Fatalf("byKey entry's meta: got %T, want map[string]any", entry["meta"])
	}
	request, ok := meta["request"].(map[string]any)
	if !ok {
		t.Fatalf("byKey entry's meta.request: got %T, want map[string]any", meta["request"])
	}
	if _, present := request["uri"]; present {
		t.Errorf("byKey entry's meta.request.uri must not survive: got %+v", request)
	}
	if request["method"] != "GET" {
		t.Errorf("byKey entry's meta.request.method should survive untouched: got %+v", request)
	}
}

func TestApplyResultStrategy_PaginatedShapePassesThroughUnchanged(t *testing.T) {
	// fetchPaginated already rebuilds {"data":..., "truncated":...} itself and
	// never forwards links/meta — scrubbing must be a no-op here, not just
	// harmless but observably untouched (no extra keys, no panic on a step
	// with neither links nor meta present).
	stepResults := map[string]map[string]any{
		"queues": {
			"data":      []any{map[string]any{"queueName": "orders"}},
			"truncated": false,
		},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"queues": map[string]any{
			"data":      []any{map[string]any{"queueName": "orders"}},
			"truncated": false,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestApplyResultStrategy_PostProcess_StripsLinksFromRawStep(t *testing.T) {
	postprocesstest.Register(t, "__test_summary_links", postprocess.Handler{
		Fn: func(map[string]map[string]any) (map[string]any, error) {
			return map[string]any{"count": 1}, nil
		},
	})
	stepResults := map[string]map[string]any{
		"queues": {
			"data":  []any{},
			"links": map[string]any{"uri": "https://broker:943/SEMP/v2/monitor/msgVpns/default/queues"},
		},
	}
	got, err := composite.ApplyResultStrategy(
		composite.ResultStrategy{Strategy: "postProcess", PostProcess: "__test_summary_links"},
		stepResults,
	)
	if err != nil {
		t.Fatal(err)
	}
	queues := got["queues"].(map[string]any)
	if _, present := queues["links"]; present {
		t.Errorf("links must not survive a postProcess tool's raw step either: got %+v", queues["links"])
	}
}
