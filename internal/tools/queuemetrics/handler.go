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

// Package queuemetrics implements the get-queue-metrics MCP tool.
//
// It is a mixed-protocol tool: it issues a SEMPv2 monitor query and a SEMPv1
// "show queue detail" command against the same queue in parallel and merges
// them into one response:
//
//	SEMPv2 monitor/getMsgVpnQueue → "queueMetrics" (rates, config, congestion,
//	    and cumulative lifetime counters such as spooledMsgCount)
//	SEMPv1 show queue ... detail  → "liveDepth" (authoritative CURRENT depth)
//
// The reason for the split: SEMPv2 MsgVpnQueue exposes no scalar for the
// current number of messages in a queue — spooledMsgCount is a cumulative
// lifetime counter that only increases, and spooledMsgCount - deletedMsgCount
// does not track consumption (deletedMsgCount does not increment on normal
// consume+ack). SEMPv1 num-messages-spooled is the authoritative live depth
// and decreases as messages are consumed (SOL-150260). This tool was
// previously a SEMPv2-only composite tool; it is native so it can merge the
// two protocols in a single call.
package queuemetrics

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// toolName is the on-the-wire MCP tool name and the prefix used for tool-side
// error wrapping. Kept in one place so the two cannot drift.
const toolName = "get-queue-metrics"

// sempv2Select is the SEMPv2 monitor field set surfaced under "queueMetrics".
// It matches the field list this tool used when it was a composite tool, so
// the queueMetrics block's shape is unchanged for existing consumers.
// spooledMsgCount is retained deliberately (it is a useful cumulative signal
// when read as "messages ever spooled", not current depth).
const sempv2Select = "accessType, averageRxMsgRate, averageTxMsgRate, bindCount, " +
	"durable, egressEnabled, ingressEnabled, lowPriorityMsgCongestionState, " +
	"maxBindCount, maxDeliveredUnackedMsgsPerFlow, maxMsgSize, maxMsgSpoolUsage, " +
	"maxMsgSpoolUsageExceededDiscardedMsgCount, maxRedeliveryCount, " +
	"maxRedeliveryExceededDiscardedMsgCount, maxTtl, maxTtlExceededDiscardedMsgCount, " +
	"msgSpoolUsage, msgVpnName, noLocalDeliveryDiscardedMsgCount, owner, queueName, " +
	"redeliveredMsgCount, rxByteRate, rxMsgRate, spooledMsgCount, txByteRate, " +
	"txMsgRate, txUnackedMsgCount"

// Compile-time check that Handler satisfies tools.ToolHandler.
var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the get-queue-metrics MCP tool. Stateless; one instance
// is sufficient per server.
type Handler struct{}

// NewHandler returns a get-queue-metrics tool handler ready to register with a
// ToolManager.
func NewHandler() *Handler { return &Handler{} }

// Metadata describes the tool to the MCP layer. Returns a freshly allocated
// value per call so callers cannot mutate shared state.
func (h *Handler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: toolName,
		Description: "Get detailed metrics for a queue. Returns two blocks: " +
			"'liveDepth' (from SEMPv1) with the AUTHORITATIVE current queue depth — " +
			"liveDepth.currentMsgCount is the number of messages sitting in the queue " +
			"right now and decreases as they are consumed; use this for backlog/depth " +
			"questions. 'queueMetrics' (from SEMPv2) with rates, congestion state, " +
			"spool usage, redelivery counts, and configuration. IMPORTANT: the " +
			"queueMetrics.spooledMsgCount field is a cumulative lifetime counter " +
			"(messages ever spooled since creation / last stats clear) — it only " +
			"increases and is NOT the current depth; do not report it as the current " +
			"backlog. Note queueMetrics.msgSpoolUsage is in bytes while " +
			"maxMsgSpoolUsage is in megabytes. Primary signal for a slow " +
			"guaranteed-message consumer: liveDepth.currentMsgCount rising across " +
			"successive calls AND bindCount > 0 AND rxMsgRate > txMsgRate AND " +
			"txUnackedMsgCount near the per-flow unacked limit. The per-client " +
			"slowSubscriber field does not flip for slow ACKs, so this tool — not " +
			"get-client-details — is the right starting point.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msgVpnName": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "The Message VPN containing the queue",
				},
				"queueName": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "The name of the queue",
				},
			},
			"required": []string{"msgVpnName", "queueName"},
		},
		OutputSchema: tools.StepKeyedEnvelopeSchema(),
		Annotations:  tools.ReadOnlyAnnotations(),
	}
}

// buildSEMPv2Operation returns the monitor/getMsgVpnQueue operation. Native
// handlers do not receive the parsed OpenAPI catalog, so the operation is
// declared inline (same pattern as the queueactions handlers). The path is the
// private monitor endpoint because fields like bindCount are not available on
// the public monitor endpoint.
func buildSEMPv2Operation() *sempv2.Operation {
	return &sempv2.Operation{
		ID:     "getMsgVpnQueue",
		Method: "GET",
		Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/queues/{queueName}",
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "queueName", In: "path", Type: "string", Required: true},
			{Name: "select", In: "query", Type: "array"},
		},
	}
}

// sempv1DetailXML builds the SEMPv1 request for a single queue's detail. The
// queue and VPN names are XML-escaped defensively; the schema's minLength:1
// already rejects empty values upstream.
func sempv1DetailXML(msgVpnName, queueName string) (string, error) {
	qb, err := xmlEscape(queueName)
	if err != nil {
		return "", err
	}
	vb, err := xmlEscape(msgVpnName)
	if err != nil {
		return "", err
	}
	return "<rpc><show><queue><name>" + qb + "</name><vpn-name>" +
		vb + "</vpn-name><detail/></queue></show></rpc>", nil
}

// Handle issues the SEMPv2 monitor query and the SEMPv1 detail command in
// parallel and merges them. Partial-failure policy mirrors get-broker-status:
// if either call fails, the whole tool fails, because a metrics snapshot that
// silently drops the live depth (or vice versa) would mislead the caller.
// errgroup's first-error-cancels semantics give us this for free.
func (h *Handler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	msgVpnName, _ := params["msgVpnName"].(string)
	queueName, _ := params["queueName"].(string)
	if msgVpnName == "" {
		return nil, fmt.Errorf("%s: msgVpnName is required", toolName)
	}
	if queueName == "" {
		return nil, fmt.Errorf("%s: queueName is required", toolName)
	}

	// Pre-allocated slots — each goroutine writes its own, so no lock needed.
	var (
		queueMetrics map[string]any
		liveDepth    map[string]any
	)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		op := buildSEMPv2Operation()
		args := map[string]any{
			"msgVpnName": msgVpnName,
			"queueName":  queueName,
			"select":     sempv2Select,
		}
		result, err := tc.SEMPv2Client.Execute(gCtx, op, args)
		if err != nil {
			return err
		}
		queueMetrics = result.Data
		return nil
	})

	g.Go(func() error {
		reqXML, err := sempv1DetailXML(msgVpnName, queueName)
		if err != nil {
			return fmt.Errorf("%s: building SEMPv1 request: %w", toolName, err)
		}
		result, err := tc.SEMPv1Client.Execute(gCtx, reqXML)
		if err != nil {
			return err
		}
		var resp queueDetailResponse
		if err := xml.Unmarshal(result.InnerXML, &resp); err != nil {
			return fmt.Errorf("%s: parsing queue detail response: %w", toolName, err)
		}
		liveDepth, err = structToMap(resp.Info)
		if err != nil {
			return err
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"queueMetrics": queueMetrics,
			"liveDepth":    liveDepth,
		},
	}, nil
}

// structToMap round-trips v through json.Marshal / json.Unmarshal so the
// resulting map honors json: tags and ,omitempty (mirrors the brokerstatus
// helper). A nil *queueInfo yields an empty map rather than a JSON null, so
// the liveDepth key is always a well-formed object.
func structToMap(v *queueInfo) (map[string]any, error) {
	out := map[string]any{}
	if v == nil {
		return out, nil
	}
	asJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: marshalling live depth to JSON: %w", toolName, err)
	}
	if err := json.Unmarshal(asJSON, &out); err != nil {
		return nil, fmt.Errorf("%s: parsing live depth JSON: %w", toolName, err)
	}
	return out, nil
}

// xmlEscape returns the XML-escaped form of s for safe interpolation into the
// SEMPv1 request body.
func xmlEscape(s string) (string, error) {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "", err
	}
	return buf.String(), nil
}
