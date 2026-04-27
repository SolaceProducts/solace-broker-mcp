package sempv1

import (
	"testing"
)

func TestGetRedundancyStatus_Name(t *testing.T) {
	h := NewGetRedundancyStatusHandler()
	if got := h.Name(); got != "get_redundancy_status" {
		t.Errorf("Name() = %q, want %q", got, "get_redundancy_status")
	}
}

func TestGetRedundancyStatus_Schema_NoParams(t *testing.T) {
	h := NewGetRedundancyStatusHandler()
	schema := h.Schema()

	if schema["type"] != "object" {
		t.Errorf(`schema["type"] = %v, want "object"`, schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf(`schema["properties"] is not a map[string]any: %T`, schema["properties"])
	}
	if len(props) != 0 {
		t.Errorf("schema has %d properties, want 0 (broker is injected by ToolManager)", len(props))
	}
	// No "required" key expected since there are no params.
	if _, hasRequired := schema["required"]; hasRequired {
		t.Error(`schema["required"] should not be set when properties is empty`)
	}
}

func TestGetRedundancyStatus_OutputSchema_GenericEnvelope(t *testing.T) {
	h := NewGetRedundancyStatusHandler()
	out := h.OutputSchema()

	if out["type"] != "object" {
		t.Errorf(`OutputSchema()["type"] = %v, want "object"`, out["type"])
	}
	addProps, ok := out["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatalf(`OutputSchema()["additionalProperties"] is not a map[string]any: %T`,
			out["additionalProperties"])
	}
	if addProps["type"] != "object" {
		t.Errorf(`additionalProperties["type"] = %v, want "object"`, addProps["type"])
	}
}

func TestGetRedundancyStatus_Annotations_ReadOnly(t *testing.T) {
	h := NewGetRedundancyStatusHandler()
	ann := h.Annotations()

	if ann == nil {
		t.Fatal("Annotations() returned nil")
	}
	if !ann.ReadOnlyHint {
		t.Error("ReadOnlyHint = false, want true")
	}
	if ann.DestructiveHint == nil || *ann.DestructiveHint {
		t.Errorf("DestructiveHint = %v, want explicit false", ann.DestructiveHint)
	}
}

func TestGetRedundancyStatus_Description_Nonempty(t *testing.T) {
	h := NewGetRedundancyStatusHandler()
	if h.Description() == "" {
		t.Error("Description() returned empty string")
	}
}
