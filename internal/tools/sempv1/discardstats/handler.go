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

// Package discardstats implements the get-discard-stats MCP tool.
//
// The tool returns pre-aggregated discard counters from SEMPv1. Two modes:
//
//	vpnName omitted → broker-wide
//	   <rpc><show><stats><client/></stats></show></rpc>             → "clientDiscards"
//	   <rpc><show><message-spool><stats/></message-spool></show></rpc> → "spoolDiscards"
//
//	vpnName provided → per-VPN
//	   <rpc><show><message-vpn><vpn-name>{vpnName}</vpn-name><stats/></message-vpn></show></rpc>
//	      → "clientDiscards" only (broker does not expose per-VPN spool discards)
//
// SEMPv2's queue-level select(...) is not used here because SEMPv2 has no
// broker-level aggregate view. The companion list-queue-discards composite
// tool covers per-queue inspection.
//
// Curated field selection is documented in
// docs/internal/semp/get-discard-stats-curated-fields.md.
package discardstats

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/safego"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"golang.org/x/sync/errgroup"
)

const toolName = "get-discard-stats"

// outputSchema describes both response shapes the tool can return.
// Broker-wide:  { clientDiscards, spoolDiscards }
// Per-VPN:      { vpnName, clientDiscards }
// The schema strictly bounds the response: clientDiscards is the cross-mode
// invariant (required), and additionalProperties: false rejects any leaked or
// debugging field that isn't explicitly enumerated here.
func outputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"vpnName":        map[string]any{"type": "string"},
			"clientDiscards": map[string]any{"type": "object"},
			"spoolDiscards":  map[string]any{"type": "object"},
		},
		"required":             []string{"clientDiscards"},
		"additionalProperties": false,
	}
}

// Broker-wide RPC requests — static.
const (
	statsClientXML = `<rpc><show><stats><client/></stats></show></rpc>`
	spoolStatsXML  = `<rpc><show><message-spool><stats/></message-spool></show></rpc>`
)

// vpnStatsXML returns the per-VPN client-stats RPC. vpnName is XML-escaped
// via sempv1.Escape (the package-level helper mandated for all
// externally-sourced values before concatenation into a SEMPv1 request)
// to neutralize any embedded markup before interpolation.
func vpnStatsXML(vpnName string) string {
	return fmt.Sprintf(
		`<rpc><show><message-vpn><vpn-name>%s</vpn-name><stats/></message-vpn></show></rpc>`,
		sempv1.Escape(vpnName),
	)
}

var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the get-discard-stats MCP tool.
type Handler struct{}

// NewHandler returns a discard-stats tool handler ready to register with a
// ToolManager.
func NewHandler() *Handler {
	return &Handler{}
}

// Metadata describes the tool to the MCP layer. The vpnName parameter is
// optional — omitting it returns broker-wide aggregates; providing it scopes
// the result to a single VPN.
func (h *Handler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: toolName,
		Description: "Returns pre-aggregated message discard counters. " +
			"Important: SEMPv1 exposes spool-level discards (max-redelivery " +
			"exceeded, TTL expired to DMQ, spool/queue over quota, " +
			"max-msg-size exceeded, replication-standby discards, etc.) " +
			"only at broker-wide scope — there is no per-VPN spool " +
			"breakdown. To answer \"how many messages did VPN X drop at " +
			"the spool/queue layer?\", use list-queue-discards instead. " +
			"Without vpnName: returns broker-wide totals — client-level " +
			"discards (ingress/egress: no-subscription-match, topic-parse " +
			"errors, msg-too-big, TTL exceeded, transmit congestion, " +
			"client-not-connected, etc.) plus the broker-wide spool-level " +
			"discards. With vpnName: returns only client-level discards " +
			"scoped to that VPN. Use this for broker-level health checks " +
			"(\"are we dropping messages anywhere?\").",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"vpnName": map[string]any{
					"type":      "string",
					"minLength": 1,
					"description": "Optional. If provided, scopes the discard " +
						"counts to a single Message VPN (client-level only). " +
						"If omitted, returns broker-wide totals including " +
						"spool-level discards. Must be non-empty when " +
						"present — empty strings are rejected so the tool " +
						"never silently downgrades to broker-wide.",
				},
			},
		},
		OutputSchema: outputSchema(),
		Annotations:  tools.ReadOnlyAnnotations(),
	}
}

// Handle dispatches to broker-wide or per-VPN mode based on the vpnName
// parameter. Errors from the SEMPv1 client are returned unwrapped so the
// manager's errors.As path can extract *sempv1.Error. XML decode errors are
// wrapped with the tool-name + step prefix.
func (h *Handler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	if vpnRaw, ok := params["vpnName"]; ok {
		vpnName, _ := vpnRaw.(string)
		if vpnName != "" {
			return h.handleVPN(ctx, tc.SEMPv1Client, vpnName)
		}
	}
	return h.handleBrokerWide(ctx, tc.SEMPv1Client)
}

// handleBrokerWide issues the two broker-wide RPCs in parallel and returns
// the combined envelope. Partial-failure policy: if either call fails, the
// whole tool fails — a partial broker-wide picture is misleading.
func (h *Handler) handleBrokerWide(ctx context.Context, client sempv1.Client) (*tools.ToolResult, error) {
	var (
		clientResp clientStatsResponse
		spoolResp  spoolStatsResponse
	)

	g, gCtx := errgroup.WithContext(ctx)
	safego.Go(g, func() error {
		return executeAndDecode(gCtx, client, statsClientXML, "client", &clientResp)
	})
	safego.Go(g, func() error {
		return executeAndDecode(gCtx, client, spoolStatsXML, "spool", &spoolResp)
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	clientMap, err := structToMap(clientResp.IngressEgressDiscards(), "clientDiscards")
	if err != nil {
		return nil, err
	}
	spoolMap, err := structToMap(spoolResp.Discards(), "spoolDiscards")
	if err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"clientDiscards": clientMap,
			"spoolDiscards":  spoolMap,
		},
	}, nil
}

// handleVPN issues the per-VPN client-stats RPC and returns the curated
// ingress/egress discard counters scoped to that VPN.
func (h *Handler) handleVPN(ctx context.Context, client sempv1.Client, vpnName string) (*tools.ToolResult, error) {
	var resp vpnStatsResponse
	if err := executeAndDecode(ctx, client, vpnStatsXML(vpnName), "vpn-stats", &resp); err != nil {
		return nil, err
	}

	clientMap, err := structToMap(resp.IngressEgressDiscards(), "clientDiscards")
	if err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"vpnName":        vpnName,
			"clientDiscards": clientMap,
		},
	}, nil
}

// executeAndDecode issues a single SEMPv1 RPC and decodes the inner
// <show>...</show> bytes into target. The expected inner-tag for each
// supported target type is hard-coded in the switch below. label is used
// only to attribute parse errors back to the calling tool step.
func executeAndDecode(
	ctx context.Context,
	client sempv1.Client,
	xmlReq, label string,
	target any,
) error {
	result, err := client.Execute(ctx, xmlReq)
	if err != nil {
		return err
	}

	switch t := target.(type) {
	case *clientStatsResponse:
		var w struct {
			XMLName xml.Name            `xml:"show"`
			Inner   clientStatsResponse `xml:"stats"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, label, err)
		}
		*t = w.Inner
	case *spoolStatsResponse:
		var w struct {
			XMLName xml.Name           `xml:"show"`
			Inner   spoolStatsResponse `xml:"message-spool"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, label, err)
		}
		*t = w.Inner
	case *vpnStatsResponse:
		var w struct {
			XMLName xml.Name         `xml:"show"`
			Inner   vpnStatsResponse `xml:"message-vpn"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, label, err)
		}
		*t = w.Inner
	default:
		return fmt.Errorf("%s: internal: unsupported response type %T", toolName, target)
	}

	return nil
}

// structToMap round-trips v through json.Marshal/Unmarshal so the resulting
// map honors json: tags and omitempty.
func structToMap(v any, label string) (map[string]any, error) {
	asJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: marshalling %s to JSON: %w", toolName, label, err)
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		return nil, fmt.Errorf("%s: parsing %s JSON: %w", toolName, label, err)
	}
	return out, nil
}
