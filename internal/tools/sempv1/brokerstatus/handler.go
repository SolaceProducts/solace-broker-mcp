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
// On appliance brokers a fifth, conditional, sequential call also runs:
//
//	show hardware details              → "hardwareDetails" key (appliance only)
//
// The hardware step fires only when the show-version description identifies
// the broker as an appliance (see isApplianceFromDescription). The step is
// failure-isolated — a transport or parse error there is logged and the
// hardwareDetails section is omitted, but the rest of the envelope still
// returns. Software / cloud brokers skip the call entirely.
//
// Field selection is operator-driven, not exhaustive — see
// docs/internal/semp/get-broker-status-curated-fields.md for the rationale,
// source citations, and full curated list.
package brokerstatus

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/safego"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	"golang.org/x/sync/errgroup"
)

// toolName is the prefix used for tool-side error wrapping. Centralizing
// it prevents typos and keeps the prefix in lockstep with Metadata().Name.
const toolName = "get-broker-status"

// Static SEMPv1 request strings — declared here rather than inline in Handle
// so the call shape is visible at a glance. The first four run in parallel
// on every invocation; hardwareXML is conditional and sequential.
const (
	versionXML  = `<rpc><show><version/></show></rpc>`
	systemXML   = `<rpc><show><system/></show></rpc>`
	memoryXML   = `<rpc><show><memory/></show></rpc>`
	spoolXML    = `<rpc><show><message-spool><detail/></message-spool></show></rpc>`
	hardwareXML = `<rpc><show><hardware><details/></hardware></show></rpc>`
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
			"memory and message-spool utilization, and on hardware appliances " +
			"the chassis identity and physical-component inventory (CPU, memory, " +
			"power, disks, blades). Reports raw state, not a health verdict - " +
			"whether the broker is \"healthy\" depends on deployment intent " +
			"(e.g. message-spool disabled is expected for direct-messaging-only " +
			"deployments, not a fault). Use whenever the user asks about a " +
			"broker's status or health - whether it is healthy, slow, restarted, " +
			"under-scaled, low on capacity, what hardware it runs on, or before " +
			"maintenance. Specify the target broker by its configured alias.",
		InputSchema:  tools.EmptyObjectSchema(),
		OutputSchema: tools.StepKeyedEnvelopeSchema(),
		Annotations:  tools.ReadOnlyAnnotations(),
	}
}

// Handle issues the four core SEMPv1 calls in parallel, decodes each response
// into the curated typed struct, and assembles a step-keyed envelope under
// the keys "version", "system", "memory", and "spool". On appliance brokers
// a conditional fifth call (show hardware details) runs sequentially after
// the parallel fan-out and adds a "hardwareDetails" key.
//
// Errors from the SEMPv1 client are returned unwrapped so the manager's
// errors.As path can extract *sempv1.Error for structured logging. Errors
// from XML or JSON processing inside this function are wrapped with a
// tool-name prefix so log lines distinguish broker-side failures from
// tool-side processing failures.
//
// Partial-failure policy:
//
//   - The four parallel calls are all-or-nothing: if any one fails, the whole
//     tool fails. Broker status is a coherent picture and a partial envelope
//     could mislead downstream consumers (operators, LLMs, dashboards).
//     errgroup's first-error-cancels semantics give us this for free.
//   - The conditional hardware step is best-effort and failure-isolated:
//     a transport or parse error is logged with structured fields and the
//     hardwareDetails section is omitted, but the rest of the envelope still
//     returns. The other four sections are a complete answer for software
//     brokers, so an appliance-only failure must not fail the whole tool.
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

	safego.Go(g, func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, versionXML, "version", &versionResp)
	})
	safego.Go(g, func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, systemXML, "system", &systemResp)
	})
	safego.Go(g, func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, memoryXML, "memory", &memoryResp)
	})
	safego.Go(g, func() error {
		return executeAndDecode(gCtx, tc.SEMPv1Client, spoolXML, "message-spool", &spoolResp)
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Conditional fifth step — only fires on appliance brokers, runs
	// sequentially after the parallel fan-out succeeds. The check is
	// gated on the show-version description (already populated above) so
	// software / cloud brokers cost zero extra round-trips.
	//
	// Failure isolation: an error here is logged and dropped. The other
	// four sections are a complete answer for software brokers, so this
	// step's failure must not fail the whole tool. Appliance operators
	// will notice the missing hardwareDetails section in the response.
	var hwCurated *hardwareCurated
	if versionResp.Description != nil && isApplianceFromDescription(*versionResp.Description) {
		var hwResp hardwareResponse
		if err := executeAndDecode(ctx, tc.SEMPv1Client, hardwareXML, "hardware", &hwResp); err != nil {
			logHardwareStepFailure(ctx, err)
		} else {
			hwCurated = hwResp.Curated()
		}
	}

	envelope, err := assembleEnvelope(versionResp, systemResp, memoryResp, spoolResp, hwCurated)
	if err != nil {
		return nil, err
	}

	return &tools.ToolResult{StructuredContent: envelope}, nil
}

// logHardwareStepFailure emits a structured warn record for a failed
// hardware-details step. The raw err.Error() is intentionally NOT logged —
// per docs/internal/secure-logging-rules.md (Rule 5), errors from external
// systems must be logged via structured fields, not as a free-form string.
// When the underlying error is a *sempv1.Error we expose its kind,
// http_status, and reason_code (the same fields manager.logToolResult emits
// for tool-level failures, so log shape stays consistent across tools);
// otherwise we surface only the Go error type, never the message.
//
// For the non-*sempv1.Error path we unwrap to the deepest cause before
// recording the type. executeAndDecode wraps XML parse errors via
// fmt.Errorf %w, so logging %T on the outer error would always read
// "*fmt.wrapError" — useless for triage. Unwrapping surfaces the real
// type (e.g. "*xml.SyntaxError") while still avoiding the message.
func logHardwareStepFailure(ctx context.Context, err error) {
	attrs := []slog.Attr{
		slog.String("tool", toolName),
		slog.String("step", "hardware"),
	}
	var v1Err *sempv1.Error
	if errors.As(err, &v1Err) {
		attrs = append(attrs,
			slog.String("error_type", "*sempv1.Error"),
			slog.String("kind", v1Err.Kind.String()),
			slog.Int("http_status", v1Err.StatusCode),
			slog.Int("reason_code", v1Err.ReasonCode))
	} else {
		root := err
		for {
			u := errors.Unwrap(root)
			if u == nil {
				break
			}
			root = u
		}
		attrs = append(attrs, slog.String("error_type", fmt.Sprintf("%T", root)))
	}
	slog.LogAttrs(ctx, slog.LevelWarn,
		"hardware-details step failed; omitting hardwareDetails section", attrs...)
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
	case *hardwareResponse:
		var w struct {
			XMLName xml.Name         `xml:"show"`
			Inner   hardwareResponse `xml:"hardware"`
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
// and stitches them into the step envelope. The JSON round-trip honors json:
// tags (camelCase + omitempty), so absent broker fields drop out of the
// output naturally.
//
// hw is optional — nil when the broker is non-appliance (skip path) or when
// the conditional hardware step failed (best-effort drop). On nil the
// hardwareDetails key is omitted entirely rather than emitted as an empty
// object, so consumers can use simple key presence to distinguish "appliance
// with data" from "no hardware data available".
func assembleEnvelope(
	v versionResponse,
	s systemResponse,
	m memoryResponse,
	sp messageSpoolResponse,
	hw *hardwareCurated,
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

	envelope := map[string]any{
		"version": versionMap,
		"system":  systemMap,
		"memory":  memoryMap,
		"spool":   spoolMap,
	}

	if hw != nil {
		hwMap, err := structToMap(hw, "hardwareDetails")
		if err != nil {
			return nil, err
		}
		envelope["hardwareDetails"] = hwMap
	}

	return envelope, nil
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
