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

package handlers

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The item builders below use each tool's FULL select: field list from
// tools.yaml (not the minimal per-test helpers like vpn()/queue()/rdp()/
// bridge() in the sibling _test.go files, which only carry the fields their
// classification-logic tests need) — marshal cost scales with the real
// payload shape, and a minimal item would understate it.

func benchVpnItem(i int) map[string]any {
	return map[string]any{
		"discardedRxMsgCount":                       int64(i * 3),
		"discardedTxMsgCount":                       int64(i * 2),
		"dmrEnabled":                                i%2 == 0,
		"enabled":                                   true,
		"msgSpoolMsgCount":                          int64(i * 1000),
		"msgSpoolUsage":                             float64(i * 4096),
		"msgVpnConnections":                         i % 50,
		"msgVpnConnectionsServiceMqtt":              i % 10,
		"msgVpnConnectionsServiceRestIncoming":      i % 5,
		"msgVpnConnectionsServiceSmf":               i % 30,
		"msgVpnConnectionsServiceWeb":               i % 5,
		"msgVpnName":                                fmt.Sprintf("vpn-%d", i),
		"msgVpnTotalUniqueSubscriptions":            i * 7,
		"replicationEnabled":                        false,
		"serviceMqttPlainTextFailureReason":         "",
		"serviceMqttPlainTextUp":                    true,
		"serviceRestIncomingPlainTextFailureReason": "",
		"serviceRestIncomingPlainTextUp":            true,
		"serviceSmfPlainTextFailureReason":          "",
		"serviceSmfPlainTextUp":                     true,
		"state":                                     "up",
	}
}

func benchQueueItem(i int) map[string]any {
	return map[string]any{
		"accessType":                    "non-exclusive",
		"bindCount":                     i % 5,
		"egressEnabled":                 true,
		"ingressEnabled":                true,
		"lowPriorityMsgCongestionState": "not-congested",
		"maxMsgSpoolUsage":              float64(1000),
		"msgSpoolUsage":                 float64(i * 4096),
		"msgVpnName":                    "default",
		"queueName":                     fmt.Sprintf("queue-%d", i),
		"rxMsgRate":                     float64(i % 100),
		"spooledMsgCount":               int64(i * 500),
		"txMsgRate":                     float64(i % 90),
		"txUnackedMsgCount":             i % 10,
	}
}

func benchClientItem(i int) map[string]any {
	return map[string]any{
		"clientAddress":  fmt.Sprintf("10.0.%d.%d:%d", i%256, (i/256)%256, 40000+i%20000),
		"clientName":     fmt.Sprintf("client-%d", i),
		"clientUsername": fmt.Sprintf("user-%d", i%20),
		"msgVpnName":     "default",
		"platform":       "JCSMP/Java 1.13.2 (JCSMP)",
		"rxMsgRate":      float64(i % 100),
		"slowSubscriber": i%37 == 0,
		"txMsgRate":      float64(i % 90),
		"uptime":         int64(i * 60),
	}
}

func benchRdpItem(i int) map[string]any {
	return map[string]any{
		"clientName":            fmt.Sprintf("rdp-client-%d", i),
		"enabled":               true,
		"lastFailureReason":     "",
		"lastFailureTime":       "",
		"msgVpnName":            "default",
		"restDeliveryPointName": fmt.Sprintf("rdp-%d", i),
		"up":                    true,
	}
}

func benchBridgeItem(i int) map[string]any {
	return map[string]any{
		"bridgeName":           fmt.Sprintf("bridge-%d", i),
		"bridgeVirtualRouter":  "auto",
		"enabled":              true,
		"inboundFailureReason": "",
		"inboundState":         "ready-in-sync",
		"msgVpnName":           "default",
		"outboundState":        "ready",
		"remoteMsgVpnName":     "default",
		"remoteRouterName":     fmt.Sprintf("v:remote-broker-%d", i),
		"uptime":               int64(i * 60),
	}
}

// buildEnvelope assembles the same shape the composite executor sends over
// the wire for a postProcess tool: one key per step (each {"data": [...],
// "truncated": bool}) plus "summary". This is what actually gets
// json.Marshal'd for a real tool response, so it's what the benchmark times
// — not just the summary computation, which the ticket found takes 0.9ms
// for 5,000 items versus marshalling's 31ms and is already covered by the
// correctness unit tests in this package.
func buildEnvelope(stepID string, items []any, summary map[string]any) map[string]any {
	return map[string]any{
		stepID: map[string]any{
			"data":      items,
			"truncated": false,
		},
		"summary": summary,
	}
}

func benchmarkMarshal(b *testing.B, buildItem func(int) map[string]any, n int, summarize func(items []any) (map[string]any, error), stepID string) {
	items := make([]any, n)
	for i := range items {
		items[i] = buildItem(i)
	}
	summary, err := summarize(items)
	if err != nil {
		b.Fatalf("summarize: %v", err)
	}
	envelope := buildEnvelope(stepID, items, summary)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(envelope); err != nil {
			b.Fatalf("marshal: %v", err)
		}
	}
}

func BenchmarkMarshal_ListVpns(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchmarkMarshal(b, benchVpnItem, n, func(items []any) (map[string]any, error) {
				byKey := make(map[string]any, len(items))
				for _, raw := range items {
					name := raw.(map[string]any)["msgVpnName"].(string)
					byKey[name] = map[string]any{"data": []any{}}
				}
				return ListVpns(map[string]map[string]any{
					listVpnsStepID:    {"data": items},
					listVpnsClientsID: {"byKey": byKey},
				})
			}, listVpnsStepID)
		})
	}
}

func BenchmarkMarshal_ListQueues(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchmarkMarshal(b, benchQueueItem, n, func(items []any) (map[string]any, error) {
				return ListQueues(map[string]map[string]any{listQueuesStepID: {"data": items}})
			}, listQueuesStepID)
		})
	}
}

func BenchmarkMarshal_ListRdps(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchmarkMarshal(b, benchRdpItem, n, func(items []any) (map[string]any, error) {
				return ListRdps(map[string]map[string]any{listRdpsStepID: {"data": items}})
			}, listRdpsStepID)
		})
	}
}

func BenchmarkMarshal_ListBridges(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchmarkMarshal(b, benchBridgeItem, n, func(items []any) (map[string]any, error) {
				return ListBridges(map[string]map[string]any{listBridgesStepID: {"data": items}})
			}, listBridgesStepID)
		})
	}
}

// BenchmarkMarshal_ListClients covers list-clients' "collect" strategy — no
// postprocess summary, so the envelope is just the raw step data, matching
// what the executor actually sends for this tool (see tools.yaml: list-clients
// is the one list tool without a postProcess handler today).
func BenchmarkMarshal_ListClients(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			items := make([]any, n)
			for i := range items {
				items[i] = benchClientItem(i)
			}
			envelope := map[string]any{
				"clients": map[string]any{"data": items},
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := json.Marshal(envelope); err != nil {
					b.Fatalf("marshal: %v", err)
				}
			}
		})
	}
}
