// Package brokerstatus implements the get-broker-status MCP tool.
//
// The tool issues four SEMPv1 commands in parallel against the target broker
// and returns a curated subset of the responses in a step-keyed envelope:
//
//	show version                       → "version" key
//	show system                        → "system" key
//	show memory                        → "memory" key
//	show message-spool detail          → "spool" key
//
// Field selection is operator-driven, not exhaustive — see
// docs/internal/semp/get-broker-status-curated-fields.md for the rationale,
// source citations, and full curated list.
package brokerstatus

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"golang.org/x/sync/errgroup"
)

// toolName is the prefix used for tool-side error wrapping. Centralizing
// it prevents typos and keeps the prefix in lockstep with Metadata().Name.
const toolName = "get-broker-status"

// Static SEMPv1 request strings — declared here rather than inline in Handle
// so the four-call shape is visible at a glance.
const (
	versionXML = `<rpc><show><version/></show></rpc>`
	systemXML  = `<rpc><show><system/></show></rpc>`
	memoryXML  = `<rpc><show><memory/></show></rpc>`
	spoolXML   = `<rpc><show><message-spool><detail/></message-spool></show></rpc>`
)

// Compile-time check that Handler satisfies tools.ToolHandler.
var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the get-broker-status MCP tool. The handler holds no
// state; one instance is sufficient per server.
type Handler struct{}

// NewHandler returns a broker-status tool handler ready to register with a
// ToolManager.
func NewHandler() *Handler {
	return &Handler{}
}

// Metadata describes the tool to the MCP layer. Returns a freshly allocated
// value per call so the manager and registration layer cannot mutate shared
// state. The output schema is the generic step-keyed envelope; field shape
// inside each step is documented in the curated-fields design doc rather
// than in the schema, since the broker may omit any optional field.
func (h *Handler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: toolName,
		Description: "Returns a curated point-in-time status snapshot of a Solace " +
			"broker's operational state - edition and version, uptime and restart " +
			"reason, scaling limits and resource headroom (to flag under-scaling), " +
			"memory and message-spool utilization. Reports raw state, not a " +
			"health verdict - whether the broker is \"healthy\" depends on " +
			"deployment intent (e.g. message-spool disabled is expected for " +
			"direct-messaging-only deployments, not a fault). Use whenever the " +
			"user asks about a broker's status or health - whether it is " +
			"healthy, slow, restarted, under-scaled, low on capacity, or before " +
			"maintenance. Specify the target broker by its configured alias.",
		InputSchema:  tools.EmptyObjectSchema(),
		OutputSchema: tools.StepKeyedEnvelopeSchema(),
		Annotations:  tools.ReadOnlyAnnotations(),
	}
}

// Handle issues the four SEMPv1 calls in parallel, decodes each response into
// the curated typed struct, and assembles a step-keyed envelope under the
// keys "version", "system", "memory", and "spool".
//
// Errors from the SEMPv1 client are returned unwrapped so the manager's
// errors.As path can extract *sempv1.Error for structured logging. Errors
// from XML or JSON processing inside this function are wrapped with a
// tool-name prefix so log lines distinguish broker-side failures from
// tool-side processing failures.
//
// Partial-failure policy: if any one of the four calls fails, the whole tool
// fails. The broker's status is a coherent picture; returning a partial
// envelope could mislead downstream consumers (operators, LLMs, dashboards).
// errgroup's first-error-cancels semantics give us this for free.
func (h *Handler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	// Pre-allocated slots — each goroutine writes its own, so no lock needed.
	var (
		versionResp versionResponse
		systemResp  systemResponse
		memoryResp  memoryResponse
		spoolResp   messageSpoolResponse
	)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, versionXML, "version", &versionResp)
	})
	g.Go(func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, systemXML, "system", &systemResp)
	})
	g.Go(func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, memoryXML, "memory", &memoryResp)
	})
	g.Go(func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, spoolXML, "message-spool", &spoolResp)
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	envelope, err := assembleEnvelope(versionResp, systemResp, memoryResp, spoolResp)
	if err != nil {
		return nil, err
	}

	return &tools.ToolResult{StructuredContent: envelope}, nil
}

// executeAndDecode issues a single SEMPv1 RPC, then decodes the inner
// <show><innerTag>...</innerTag></show> bytes into target.
//
// Execute errors are returned unwrapped (preserving *sempv1.Error). XML
// decode errors are wrapped with the tool-name + step prefix so log lines
// pinpoint which call's parsing failed.
func executeAndDecode(ctx context.Context, client sempv1.Client, xmlReq, innerTag string, target any) error {
	result, err := client.Execute(ctx, xmlReq)
	if err != nil {
		return err
	}

	// result.InnerXML contains the inner content of <rpc>...</rpc> from the
	// broker's <rpc-reply>; parseReply already stripped the envelope. The
	// content is <show><{innerTag}>...</{innerTag}></show>, so we decode it
	// with a typed wrapper per call site. Four cases — keeping types static
	// avoids a reflect dependency for a small fixed set.
	switch t := target.(type) {
	case *versionResponse:
		var w struct {
			XMLName xml.Name        `xml:"show"`
			Inner   versionResponse `xml:"version"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, innerTag, err)
		}
		*t = w.Inner
	case *systemResponse:
		var w struct {
			XMLName xml.Name       `xml:"show"`
			Inner   systemResponse `xml:"system"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, innerTag, err)
		}
		*t = w.Inner
	case *memoryResponse:
		var w struct {
			XMLName xml.Name       `xml:"show"`
			Inner   memoryResponse `xml:"memory"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, innerTag, err)
		}
		*t = w.Inner
	case *messageSpoolResponse:
		var w struct {
			XMLName xml.Name             `xml:"show"`
			Inner   messageSpoolResponse `xml:"message-spool"`
		}
		if err := xml.Unmarshal(result.InnerXML, &w); err != nil {
			return fmt.Errorf("%s: parsing %s response: %w", toolName, innerTag, err)
		}
		*t = w.Inner
	default:
		return fmt.Errorf("%s: internal: unsupported response type %T", toolName, target)
	}

	return nil
}

// assembleEnvelope marshals each typed response to JSON, then to map[string]any,
// and stitches them into the four-key step envelope. The JSON round-trip
// honors json: tags (camelCase + omitempty), so absent broker fields drop
// out of the output naturally.
func assembleEnvelope(
	v versionResponse,
	s systemResponse,
	m memoryResponse,
	sp messageSpoolResponse,
) (map[string]any, error) {
	versionMap, err := structToMap(v, "version")
	if err != nil {
		return nil, err
	}
	systemMap, err := structToMap(s, "system")
	if err != nil {
		return nil, err
	}
	memoryMap, err := structToMap(m, "memory")
	if err != nil {
		return nil, err
	}
	spoolMap, err := structToMap(sp, "spool")
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"version": versionMap,
		"system":  systemMap,
		"memory":  memoryMap,
		"spool":   spoolMap,
	}, nil
}

// structToMap round-trips v through json.Marshal and json.Unmarshal so the
// resulting map honors json: tags and ,omitempty. The label is used for
// error messages so a marshal failure reports which step's response broke.
func structToMap(v any, label string) (map[string]any, error) {
	asJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: marshalling %s response to JSON: %w", toolName, label, err)
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		return nil, fmt.Errorf("%s: parsing %s JSON response: %w", toolName, label, err)
	}
	return out, nil
}
