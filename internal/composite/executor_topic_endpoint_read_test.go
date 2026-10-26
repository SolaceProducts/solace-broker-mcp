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

package composite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// SOL-155418: list-topic-endpoints and get-topic-endpoint run the real tool
// definitions against the real embedded spec, with a fake SEMP client.

func runShippedTool(t *testing.T, name string, client sempv2.Client, params map[string]any) (map[string]any, error) {
	t.Helper()
	fx := loadKafkaStatusFixture(t)
	tool, ok := fx.tools[name]
	if !ok {
		t.Fatalf("%s not found in the embedded catalog", name)
	}
	return NewCompositeExecutor(fx.ops).Execute(context.Background(), tool, client, params)
}

func vpnPreflightOK() *sempv2.Result {
	return &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": map[string]any{"msgVpnName": "vpn1"}}}
}

func vpnNotFound() error {
	return &sempv2.SEMPError{Operation: "getMsgVpn", StatusCode: 400, SEMPCode: 11, SEMPStatus: "NOT_FOUND"}
}

func makeTopicEndpointItems(start, n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"topicEndpointName":             fmt.Sprintf("te-%d", start+i),
			"bindCount":                     float64(0),
			"msgSpoolUsage":                 float64(0),
			"maxSpoolUsage":                 float64(100),
			"lowPriorityMsgCongestionState": "not-congested",
		}
	}
	return items
}

func TestListTopicEndpoints_PreflightsThenListsWithSummary(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpn", vpnPreflightOK())
	client.addResponses("getMsgVpnTopicEndpoints", pageResult(makeTopicEndpointItems(0, 2), ""))

	result, err := runShippedTool(t, "list-topic-endpoints", client, map[string]any{"msgVpnName": "vpn1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(client.calls) != 2 || client.calls[0].opID != "getMsgVpn" || client.calls[1].opID != "getMsgVpnTopicEndpoints" {
		t.Fatalf("calls = %v, want the VPN GET then the topic endpoints GET", client.calls)
	}
	args := client.calls[1].args
	if args["msgVpnName"] != "vpn1" || args["count"] != "100" {
		t.Errorf("list args = %v", args)
	}
	// The quota is maxSpoolUsage on a topic endpoint; maxMsgSpoolUsage is the
	// queue field and does not exist on MsgVpnTopicEndpoint.
	selected := strings.Split(fmt.Sprint(args["select"]), ", ")
	for _, want := range []string{"topicEndpointName", "destinationTopic", "maxSpoolUsage", "msgSpoolUsage", "bindCount", "lowPriorityMsgCongestionState", "spooledMsgCount"} {
		if !containsString(selected, want) {
			t.Errorf("select %v is missing %s", selected, want)
		}
	}
	if containsString(selected, "maxMsgSpoolUsage") {
		t.Errorf("select %v asks for the queue-only field maxMsgSpoolUsage", selected)
	}

	summary, _ := result["summary"].(map[string]any)
	if summary["noConsumerCount"] != 2 || summary["scanned"] != 2 {
		t.Errorf("summary = %v, want noConsumerCount 2 and scanned 2", summary)
	}
	te, _ := result["topicEndpoints"].(map[string]any)
	if items, _ := te["data"].([]any); len(items) != 2 {
		t.Errorf("topicEndpoints.data = %v, want 2 rows", te["data"])
	}
}

// A missing VPN fails on the preflight. Without it the list GET would answer
// with an empty list and read as "this VPN has no topic endpoints".
func TestListTopicEndpoints_MissingVPNFailsAtPreflight(t *testing.T) {
	client := newSeqMockClient()
	client.errors["getMsgVpn"] = vpnNotFound()

	_, err := runShippedTool(t, "list-topic-endpoints", client, map[string]any{"msgVpnName": "nope"})
	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) || sempErr.Operation != "getMsgVpn" {
		t.Fatalf("err = %v, want the getMsgVpn NOT_FOUND passed through", err)
	}
	if len(client.calls) != 1 {
		t.Errorf("calls = %v, want only the VPN GET", client.calls)
	}
}

func TestListTopicEndpoints_MaxResults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxResults any
		wantItems  int
	}{
		{"one", float64(1), 1},
		{"default", nil, 100},
		{"zero falls back to the default", float64(0), 100},
		{"negative falls back to the default", float64(-5), 100},
		{"five hundred", float64(500), 500},
		{"501 is clamped to 500", float64(501), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSeqMockClient()
			client.addResponses("getMsgVpn", vpnPreflightOK())
			for p := range 6 {
				client.addResponses("getMsgVpnTopicEndpoints", pageResult(makeTopicEndpointItems(p*100, 100), fmt.Sprintf("c%d", p+1)))
			}
			params := map[string]any{"msgVpnName": "vpn1"}
			if tc.maxResults != nil {
				params["maxResults"] = tc.maxResults
			}
			result, err := runShippedTool(t, "list-topic-endpoints", client, params)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			te, _ := result["topicEndpoints"].(map[string]any)
			items, _ := te["data"].([]any)
			if len(items) != tc.wantItems {
				t.Errorf("len(topicEndpoints.data) = %d, want %d", len(items), tc.wantItems)
			}
			if te["truncated"] != true {
				t.Errorf("truncated = %v, want true (more endpoints remain)", te["truncated"])
			}
			if summary, _ := result["summary"].(map[string]any); summary["truncated"] != true {
				t.Errorf("summary.truncated = %v, want true", summary["truncated"])
			}
		})
	}
}

// Following nextCursor walks every endpoint once, in order. The expected names
// come from genClient, so this fails if tools.yaml loses resumable or cursor.
func TestListTopicEndpoints_PagesByCursor(t *testing.T) {
	const total = 250
	tool, ce := shippedTool(t, "list-topic-endpoints")
	g := &genClient{total: total}
	want := make([]any, total)
	for i := range want {
		want[i] = g.val("topicEndpointName", i)
	}
	var got []any
	cursor, pages := "", 0
	for ; pages < 10; pages++ {
		params := map[string]any{"msgVpnName": "default", "maxResults": float64(100)}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := ce.Execute(context.Background(), tool, g, params)
		if err != nil {
			t.Fatal(err)
		}
		step := res["topicEndpoints"].(map[string]any)
		for _, row := range step["data"].([]any) {
			got = append(got, row.(map[string]any)["topicEndpointName"])
		}
		next, _ := step["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 2 {
		t.Errorf("finished after %d cursor follow-ups, want 2 (250 endpoints at 100 per page)", pages)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged %d endpoint names, want all %d once each, in order", len(got), total)
	}
}

func TestGetTopicEndpoint_PreflightsThenReads(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpn", vpnPreflightOK())
	client.addResponses("getMsgVpnTopicEndpoint", &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": map[string]any{
		"topicEndpointName": "te/1",
		"destinationTopic":  "a/b",
		"maxSpoolUsage":     float64(5000),
	}}})

	result, err := runShippedTool(t, "get-topic-endpoint", client, map[string]any{"msgVpnName": "vpn1", "topicEndpointName": "te/1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(client.calls) != 2 || client.calls[0].opID != "getMsgVpn" || client.calls[1].opID != "getMsgVpnTopicEndpoint" {
		t.Fatalf("calls = %v, want the VPN GET then the topic endpoint GET", client.calls)
	}
	if args := client.calls[1].args; args["msgVpnName"] != "vpn1" || args["topicEndpointName"] != "te/1" {
		t.Errorf("get args = %v", args)
	}
	te, _ := result["topicEndpoint"].(map[string]any)
	data, _ := te["data"].(map[string]any)
	if data["destinationTopic"] != "a/b" || data["maxSpoolUsage"] != float64(5000) {
		t.Errorf("topicEndpoint.data = %v, want every attribute the broker returned", data)
	}
}

// A missing VPN and a missing endpoint are different errors from different
// operations, so the caller can tell which name was wrong.
func TestGetTopicEndpoint_MissingVPNVersusMissingEndpoint(t *testing.T) {
	t.Run("missing VPN", func(t *testing.T) {
		client := newSeqMockClient()
		client.errors["getMsgVpn"] = vpnNotFound()
		_, err := runShippedTool(t, "get-topic-endpoint", client, map[string]any{"msgVpnName": "nope", "topicEndpointName": "te1"})
		var sempErr *sempv2.SEMPError
		if !errors.As(err, &sempErr) || sempErr.Operation != "getMsgVpn" {
			t.Fatalf("err = %v, want getMsgVpn NOT_FOUND", err)
		}
		if len(client.calls) != 1 {
			t.Errorf("calls = %v, want only the VPN GET", client.calls)
		}
	})
	t.Run("missing endpoint", func(t *testing.T) {
		client := newSeqMockClient()
		client.addResponses("getMsgVpn", vpnPreflightOK())
		client.errors["getMsgVpnTopicEndpoint"] = &sempv2.SEMPError{Operation: "getMsgVpnTopicEndpoint", StatusCode: 400, SEMPCode: 6, SEMPStatus: "NOT_FOUND"}
		_, err := runShippedTool(t, "get-topic-endpoint", client, map[string]any{"msgVpnName": "vpn1", "topicEndpointName": "missing"})
		var sempErr *sempv2.SEMPError
		if !errors.As(err, &sempErr) || sempErr.Operation != "getMsgVpnTopicEndpoint" {
			t.Fatalf("err = %v, want getMsgVpnTopicEndpoint NOT_FOUND", err)
		}
	})
}

// readOnly is what registers a tool with enable_write_tools off, so a Viewer
// deployment gets both. (strictParameters on the list tool is guarded by
// TestRealCatalog_StrictParametersSet in internal/tools.)
func TestTopicEndpointReadTools_AreReadOnly(t *testing.T) {
	fx := loadKafkaStatusFixture(t)
	for _, name := range []string{"list-topic-endpoints", "get-topic-endpoint"} {
		tool, ok := fx.tools[name]
		if !ok {
			t.Errorf("%s not found in the embedded catalog", name)
			continue
		}
		if tool.Annotations.ReadOnly == nil || !*tool.Annotations.ReadOnly {
			t.Errorf("%s: want readOnly true so it registers with write tools disabled", name)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
