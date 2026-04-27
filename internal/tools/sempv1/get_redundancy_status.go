package sempv1

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time check that GetRedundancyStatusHandler satisfies tools.ToolHandler.
var _ tools.ToolHandler = (*GetRedundancyStatusHandler)(nil)

// GetRedundancyStatusHandler implements the get_redundancy_status MCP tool.
// It issues a single SEMPv1 show-redundancy command against the target
// broker and returns the parsed response in a step-keyed envelope.
//
// See docs/semp/sempv1-tool-wiring-plan.md §7 for the output-shape
// rationale (envelope vs. flat curation).
type GetRedundancyStatusHandler struct{}

// NewGetRedundancyStatusHandler returns a handler ready to register with
// a ToolManager. The handler holds no state; one instance is sufficient
// per server.
func NewGetRedundancyStatusHandler() *GetRedundancyStatusHandler {
	return &GetRedundancyStatusHandler{}
}

// Name returns the tool's unique identifier used for routing by the
// ToolManager and for invocation by MCP clients.
func (h *GetRedundancyStatusHandler) Name() string {
	return "get_redundancy_status"
}

// Description returns the LLM-facing description that helps the agent
// decide when to call this tool. Follows the description guidelines in
// internal/composite/definitions/tools.yaml header comment.
func (h *GetRedundancyStatusHandler) Description() string {
	return "Returns the broker's redundancy and high-availability status, " +
		"including config/operational status, active-standby role, mate router " +
		"name, mate link state, and per-virtual-router activity. Use this tool " +
		"to assess HA health during incident triage. Single SEMPv1 call."
}

// Schema returns the JSON Schema for tool input parameters. The tool
// takes no tool-specific parameters — the broker parameter is injected
// by the ToolManager during registration and validation.
func (h *GetRedundancyStatusHandler) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// OutputSchema returns the generic step-keyed envelope schema used by
// every SEMPv1 tool: a top-level object whose keys are step IDs and
// whose values are the parsed SEMP responses. Per
// docs/semp/sempv1-tool-wiring-plan.md §7, the schema validates the
// envelope structure but not individual response fields, so post-MVP
// curation can flatten without a schema migration.
func (h *GetRedundancyStatusHandler) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "object"},
	}
}

// Annotations returns MCP tool annotations. get_redundancy_status is a
// pure read with no side effects: ReadOnlyHint=true,
// DestructiveHint=false. Idempotent and OpenWorld hints are left
// unspecified (nil) — SDK defaults apply.
func (h *GetRedundancyStatusHandler) Annotations() *mcp.ToolAnnotations {
	destructive := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &destructive,
	}
}

// Handle executes the get_redundancy_status tool: builds the
// <rpc><show><redundancy/></show></rpc> request, calls the SEMPv1 client,
// decodes the inner <show><redundancy>...</redundancy></show> bytes into
// redundancyResponse, and wraps the result in a step-keyed envelope under
// the "redundancy" key. Returns broker errors unwrapped (preserving
// *sempv1.Error for the manager's structured logging) and wraps
// XML/JSON processing errors with a tool-name prefix.
func (h *GetRedundancyStatusHandler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	xmlReq := `<rpc><show><redundancy/></show></rpc>`

	// send to broker
	result, err := tc.SEMPv1Client.Execute(ctx, xmlReq)
	if err != nil {
		return nil, err
	}

	// Step 3: parse the <redundancy> payload from result.InnerXML
	// result.InnerXML contains the inner content of <rpc>...</rpc> from the
	// broker's <rpc-reply>. parseReply (T2) already stripped the envelope.
	// The content is <show><redundancy>...</redundancy></show>, so wrap it
	// and decode with a path tag.
	var wrapper struct {
		XMLName    xml.Name           `xml:"show"`
		Redundancy redundancyResponse `xml:"redundancy"`
	}
	if err := xml.Unmarshal(result.InnerXML, &wrapper); err != nil {
		return nil, fmt.Errorf("get_redundancy_status: parsing redundancy response: %w", err)
	}

	asJSON, err := json.Marshal(wrapper.Redundancy)
	if err != nil {
		return nil, fmt.Errorf("get_redundancy_status: marshalling redundancy response to JSON: %w", err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal(asJSON, &dataMap); err != nil {
		return nil, fmt.Errorf("get_redundancy_status: parsing redundancy JSON response: %w", err)
	}

	envelope := map[string]any{
		"redundancy": dataMap,
	}

	return &tools.ToolResult{
		StructuredContent: envelope,
	}, nil
}
