// Package redundancy implements the get-redundancy-status MCP tool.
// It issues a single SEMPv1 show-redundancy command against the target
// broker and returns the parsed response in a step-keyed envelope under
// the key "redundancy".
//
// The MCP-facing tool name remains get-redundancy-status (per Story 8);
// the Go package name uses the noun form to match the show command.
package redundancy

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// Compile-time check that Handler satisfies tools.ToolHandler.
var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the get-redundancy-status MCP tool.
// It issues a single SEMPv1 show-redundancy command against the target
// broker and returns the parsed response in a step-keyed envelope.
//
// Output shape: the broker's full XML response is parsed verbatim into
// a JSON envelope under the "redundancy" key. Curation to a flat,
// curated subset of fields is post-MVP work. This per-team decision
// allows downstream consumers to start using the tool today against
// the full broker output and adopt curation later without renaming
// any fields.
type Handler struct{}

// NewHandler returns a redundancy-status tool handler ready to register
// with a ToolManager. The handler holds no state; one instance is
// sufficient per server.
func NewHandler() *Handler {
	return &Handler{}
}

// Metadata describes the tool to the MCP layer. Returns a freshly allocated
// value per call so the manager and registration layer cannot mutate shared
// state. The output schema is the generic step-keyed envelope; curation to a
// flat field set is post-MVP work tracked separately.
func (h *Handler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: "get-redundancy-status",
		Description: "Returns the broker's redundancy and high-availability status, " +
			"including config/operational status, active-standby role, mate router " +
			"name, mate link state, and per-virtual-router activity. Use this tool " +
			"to assess HA health during incident triage. Single SEMPv1 call.",
		InputSchema:  tools.EmptyObjectSchema(),
		OutputSchema: tools.StepKeyedEnvelopeSchema(),
		Annotations:  tools.ReadOnlyAnnotations(),
	}
}

// Handle executes the get-redundancy-status tool: builds the
// <rpc><show><redundancy/></show></rpc> request, calls the SEMPv1 client,
// decodes the inner <show><redundancy>...</redundancy></show> bytes into
// redundancyResponse, and wraps the result in a step-keyed envelope under
// the "redundancy" key. Returns broker errors unwrapped (preserving
// *sempv1.Error for the manager's structured logging) and wraps
// XML/JSON processing errors with a tool-name prefix.
func (h *Handler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	xmlReq := `<rpc><show><redundancy/></show></rpc>`

	result, err := tc.SEMPv1Client.Execute(ctx, xmlReq)
	if err != nil {
		return nil, err
	}

	// result.InnerXML contains the inner content of <rpc>...</rpc> from the
	// broker's <rpc-reply>. parseReply already stripped the envelope. The
	// content is <show><redundancy>...</redundancy></show>, so wrap it and
	// decode with a path tag.
	var wrapper struct {
		XMLName    xml.Name           `xml:"show"`
		Redundancy redundancyResponse `xml:"redundancy"`
	}
	if err := xml.Unmarshal(result.InnerXML, &wrapper); err != nil {
		return nil, fmt.Errorf("get-redundancy-status: parsing redundancy response: %w", err)
	}

	asJSON, err := json.Marshal(wrapper.Redundancy)
	if err != nil {
		return nil, fmt.Errorf("get-redundancy-status: marshalling redundancy response to JSON: %w", err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal(asJSON, &dataMap); err != nil {
		return nil, fmt.Errorf("get-redundancy-status: parsing redundancy JSON response: %w", err)
	}

	envelope := map[string]any{
		"redundancy": dataMap,
	}

	return &tools.ToolResult{
		StructuredContent: envelope,
	}, nil
}
