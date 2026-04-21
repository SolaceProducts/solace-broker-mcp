package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"
)

// ValidateParams validates tool input parameters against a JSON Schema (Draft 7).
// All validation errors are collected and returned in a single error message with
// schema paths, e.g.:
//
//	"parameter validation failed: (root).queueName: queueName is required;
//	 (root).maxResults: Invalid type. Expected: integer, given: string"
//
// Returns nil if validation passes.
func ValidateParams(params map[string]any, schema map[string]any) error {
	return validateAgainstSchema(params, schema, "parameter validation failed")
}

// ValidateOutput validates a tool's structured output against its output JSON
// Schema. Uses the same collection and formatting strategy as ValidateParams.
// Returns nil if validation passes.
func ValidateOutput(result map[string]any, schema map[string]any) error {
	return validateAgainstSchema(result, schema, "output validation failed")
}

// validateAgainstSchema validates a JSON document against a JSON Schema using
// gojsonschema. All errors are collected into a single message prefixed with
// the given label.
func validateAgainstSchema(document map[string]any, schema map[string]any, errPrefix string) error {
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("%s: marshalling schema: %w", errPrefix, err)
	}

	docJSON, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("%s: marshalling document: %w", errPrefix, err)
	}

	schemaLoader := gojsonschema.NewBytesLoader(schemaJSON)
	docLoader := gojsonschema.NewBytesLoader(docJSON)

	result, err := gojsonschema.Validate(schemaLoader, docLoader)
	if err != nil {
		return fmt.Errorf("%s: schema validation error: %w", errPrefix, err)
	}

	if result.Valid() {
		return nil
	}

	var msgs []string
	for _, desc := range result.Errors() {
		msgs = append(msgs, desc.String())
	}

	return fmt.Errorf("%s: %s", errPrefix, strings.Join(msgs, "; "))
}
