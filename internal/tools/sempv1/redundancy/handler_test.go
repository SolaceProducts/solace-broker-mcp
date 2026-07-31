package redundancy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// stubV1Client implements sempv1.Client for unit tests. It returns the
// configured result/err on every Execute call regardless of the XML
// argument. The xml input is not validated — handler tests focus on
// response-handling, not request shape (which is a static literal in
// Handle anyway).
type stubV1Client struct {
	result *sempv1.Result
	err    error
}

func (s *stubV1Client) Execute(ctx context.Context, xml string) (*sempv1.Result, error) {
	return s.result, s.err
}

// extractInnerRPC pulls the inner <rpc>...</rpc> bytes out of a full
// <rpc-reply> envelope, mimicking what parseReply (T2) returns at runtime.
// Used by the success test to feed Handle a realistic InnerXML.
func extractInnerRPC(t *testing.T, envelope []byte) []byte {
	t.Helper()
	s := string(envelope)
	open := strings.Index(s, "<rpc>")
	close := strings.LastIndex(s, "</rpc>")
	if open < 0 || close < 0 || close < open {
		t.Fatalf("could not locate <rpc>...</rpc> in fixture")
	}
	return []byte(s[open+len("<rpc>") : close])
}

func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "get-redundancy-status" {
		t.Errorf("Name = %q, want %q", meta.Name, "get-redundancy-status")
	}
	if meta.Description == "" {
		t.Error("Description is empty")
	}

	// Input schema: empty object (broker is injected by ToolManager).
	if meta.InputSchema["type"] != "object" {
		t.Errorf(`InputSchema["type"] = %v, want "object"`, meta.InputSchema["type"])
	}
	props, ok := meta.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf(`InputSchema["properties"] is not a map[string]any: %T`, meta.InputSchema["properties"])
	}
	if len(props) != 0 {
		t.Errorf("InputSchema has %d properties, want 0", len(props))
	}
	if _, hasRequired := meta.InputSchema["required"]; hasRequired {
		t.Error(`InputSchema["required"] should not be set when properties is empty`)
	}

	// Output schema: generic step-keyed envelope.
	if meta.OutputSchema["type"] != "object" {
		t.Errorf(`OutputSchema["type"] = %v, want "object"`, meta.OutputSchema["type"])
	}
	addProps, ok := meta.OutputSchema["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatalf(`OutputSchema["additionalProperties"] is not a map[string]any: %T`,
			meta.OutputSchema["additionalProperties"])
	}
	if addProps["type"] != "object" {
		t.Errorf(`additionalProperties["type"] = %v, want "object"`, addProps["type"])
	}

	// Annotations: read-only, explicit non-destructive.
	if !meta.Annotations.ReadOnly {
		t.Error("Annotations.ReadOnly = false, want true")
	}
	if meta.Annotations.Destructive == nil || *meta.Annotations.Destructive {
		t.Errorf("Annotations.Destructive = %v, want explicit false", meta.Annotations.Destructive)
	}
}

// TestHandle_Success runs the happy path: the stub client returns the
// inner-of-<rpc> bytes from the live-broker fixture, and Handle should
// produce a step-keyed envelope with a "redundancy" key whose value is
// a map carrying the camelCase fields decoded from the wire response.
func TestHandle_Success(t *testing.T) {
	fullEnvelope, err := os.ReadFile("testdata/show_redundancy_standalone.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	innerBytes := extractInnerRPC(t, fullEnvelope)

	stub := &stubV1Client{
		result: &sempv1.Result{InnerXML: innerBytes},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if result == nil {
		t.Fatal("Handle() returned nil result")
	}

	redundancy, ok := result.StructuredContent["redundancy"].(map[string]any)
	if !ok {
		t.Fatalf(`StructuredContent["redundancy"] is not a map: %T`,
			result.StructuredContent["redundancy"])
	}

	// Spot-check Story 8 fields appear with their wire-decoded values.
	if redundancy["configStatus"] != "Shutdown" {
		t.Errorf(`configStatus = %v, want "Shutdown"`, redundancy["configStatus"])
	}
	if redundancy["activeStandbyRole"] != "Primary" {
		t.Errorf(`activeStandbyRole = %v, want "Primary"`, redundancy["activeStandbyRole"])
	}
	if redundancy["redundancyStatus"] != "Down" {
		t.Errorf(`redundancyStatus = %v, want "Down"`, redundancy["redundancyStatus"])
	}

	// operStatus should be a nested map (proves nested-struct → nested-map round-trip works).
	operStatus, ok := redundancy["operStatus"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["operStatus"] is not a map: %T`, redundancy["operStatus"])
	}
	if operStatus["adbLinkUp"] != false {
		t.Errorf(`operStatus.adbLinkUp = %v, want false`, operStatus["adbLinkUp"])
	}
}

// TestHandle_ClientError_Passthrough verifies that errors from the
// SEMPv1 client surface unwrapped, so the manager's logToolResult (W3)
// can still extract structured fields via errors.As(err, &*sempv1.Error).
// If Handle ever wrapped these errors with anything other than %w (or
// suppressed them entirely), this test would catch it.
func TestHandle_ClientError_Passthrough(t *testing.T) {
	sempErr := &sempv1.Error{
		Kind:       sempv1.ErrorKindHTTP,
		StatusCode: 401,
	}
	stub := &stubV1Client{err: sempErr}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle() returned nil error, expected sempv1 error")
	}

	var v1Err *sempv1.Error
	if !errors.As(err, &v1Err) {
		t.Errorf("returned error %T not unwrappable to *sempv1.Error", err)
	}
	if v1Err.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", v1Err.StatusCode)
	}
}

// TestHandle_ParseError_WrapsError verifies that XML parse failures
// inside Handle are wrapped with the get-redundancy-status: prefix, so
// the resulting log line clearly attributes the failure to this tool's
// processing rather than the broker.
func TestHandle_ParseError_WrapsError(t *testing.T) {
	stub := &stubV1Client{
		result: &sempv1.Result{
			InnerXML: []byte("<not-valid-xml<<>"),
		},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle() returned nil error, expected parse failure")
	}
	if !strings.Contains(err.Error(), "redundancy response") {
		t.Errorf("error %q should mention 'redundancy response' for context", err)
	}
}
