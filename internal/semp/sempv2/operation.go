// Package sempv2 provides a client for making authenticated HTTP calls to the
// Solace SEMPv2 management API. It defines the Client interface, the Operation
// type (parsed from OpenAPI specs), and the HTTPClient implementation.
package sempv2

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
)

// Operation represents a single SEMPv2 API endpoint parsed from an OpenAPI spec.
// It holds the HTTP method, path template with parameter placeholders, and the
// parameter definitions needed to construct a valid request.
type Operation struct {
	ID          string         // operationId from OpenAPI spec (e.g., "getMsgVpnQueue")
	Method      string         // HTTP method (GET, POST, PUT, PATCH, DELETE)
	Path        string         // path template including basePath (e.g., "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}")
	Parameters  []Parameter    // path, query, and body parameters
	Description string         // human-readable description from the spec
	BodyFields map[string]bool // BodyFields is the set of attribute names the operation's request body accepts, resolved from the body parameter's
	                           // schema. It is nil when the operation has no body or its schema couldn't be resolved to a concrete property set.
}

// Parameter describes a single parameter for a SEMPv2 operation.
type Parameter struct {
	Name        string // parameter name
	In          string // "path", "query", "body"
	Type        string // "string", "integer", etc.
	Required    bool
	Description string
}

// validSpecTypes are the recognized SEMP API types derived from basePath.
var validSpecTypes = map[string]bool{
	"__private_monitor__": true,
	"__private_config__":  true,
	"__private_action__":  true,
}

// privateToPublicSpecType normalizes private basePath suffixes to their public key equivalents
// so operation map keys remain consistent (e.g. "monitor/getMsgVpnQueue") regardless of
// whether the embedded spec uses a private or public basePath.
var privateToPublicSpecType = map[string]string{
	"__private_monitor__": "monitor",
	"__private_config__":  "config",
	"__private_action__":  "action",
}

// ParseSpecs reads all embedded Swagger 2.0 JSON spec files from the given
// filesystem, extracts every operation, and returns them as a map keyed by
// "specType/operationId" (e.g., "monitor/getMsgVpnQueue"). The spec type is
// derived from each spec's basePath. This prefix disambiguates operations that
// share the same operationId across specs but target different SEMP APIs.
func ParseSpecs(fsys fs.FS) (map[string]*Operation, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading specs directory: %w", err)
	}

	operations := make(map[string]*Operation)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading spec file %q: %w", entry.Name(), err)
		}

		spec := &openapi2.T{}
		if err := spec.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("parsing spec file %q: %w", entry.Name(), err)
		}

		specType, err := deriveSpecType(spec.BasePath)
		if err != nil {
			return nil, fmt.Errorf("spec file %q: %w", entry.Name(), err)
		}

		for path, pathItem := range spec.Paths {
			methods := map[string]*openapi2.Operation{
				"GET":    pathItem.Get,
				"POST":   pathItem.Post,
				"PUT":    pathItem.Put,
				"DELETE": pathItem.Delete,
				"PATCH":  pathItem.Patch,
			}

			for method, opDef := range methods {
				if opDef == nil || opDef.OperationID == "" {
					continue
				}

				params := extractParameters(opDef, spec.Parameters)

				description := opDef.Summary
				if opDef.Description != "" {
					desc := opDef.Description
					if idx := strings.Index(desc, "Attribute|"); idx != -1 {
						desc = desc[:idx]
					}
					description = description + "\n" + desc
				}

				key := specType + "/" + opDef.OperationID
				operations[key] = &Operation{
					ID:          opDef.OperationID,
					Method:      method,
					Path:        spec.BasePath + path,
					Parameters:  params,
					Description: description,
					BodyFields:  extractBodyFields(opDef, spec.Parameters, spec.Definitions),
				}
			}
		}
	}

	if len(operations) == 0 {
		return nil, fmt.Errorf("no operations found in spec files")
	}

	return operations, nil
}

// deriveSpecType extracts the SEMP API type from a spec's basePath and normalizes
// private variants to their public equivalents (e.g. "__private_monitor__" → "monitor").
func deriveSpecType(basePath string) (string, error) {
	parts := strings.Split(strings.TrimSuffix(basePath, "/"), "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("empty basePath")
	}
	specType := parts[len(parts)-1]
	if !validSpecTypes[specType] {
		return "", fmt.Errorf("unrecognized spec type %q from basePath %q", specType, basePath)
	}
	if normalized, ok := privateToPublicSpecType[specType]; ok {
		return normalized, nil
	}
	return specType, nil
}

// extractParameters converts OpenAPI parameter definitions into the Parameter
// type used by the client for request construction. Unresolved $ref parameters
// are looked up in the spec's shared parameter definitions and resolved to
// their full definition.
func extractParameters(opDef *openapi2.Operation, sharedParams map[string]*openapi2.Parameter) []Parameter {
	var params []Parameter

	for _, p := range opDef.Parameters {
		if p.Ref != "" && p.Name == "" {
			key := strings.TrimPrefix(p.Ref, "#/parameters/")
			resolved, ok := sharedParams[key]
			if !ok {
				continue
			}
			p = resolved
		}

		param := Parameter{
			Name:        p.Name,
			In:          p.In,
			Description: p.Description,
			Required:    p.Required,
		}

		if p.In == "body" {
			param.Type = "object"
		} else if p.Type != nil && len(*p.Type) > 0 {
			param.Type = (*p.Type)[0]
		} else {
			param.Type = "string"
		}

		params = append(params, param)
	}

	return params
}

// extractBodyFields returns the set of attribute names an operation's request
// body accepts, resolved from its body parameter's schema. It returns nil when
// the operation has no body parameter or the schema can't be resolved to a
// concrete property set; callers treat nil as "unknown" and skip body
// validation, so an operation we can't introspect keeps working rather than
// having every field rejected. SEMP config definitions are flat objects (no
// allOf composition), so the schema's top-level properties are the full set.
func extractBodyFields(opDef *openapi2.Operation, sharedParams map[string]*openapi2.Parameter, definitions map[string]*openapi2.SchemaRef) map[string]bool {
	// Find the body parameter's schema, resolving a shared-parameter $ref first.
	var bodySchema *openapi2.SchemaRef
	for _, param := range opDef.Parameters {
		if param.Ref != "" && param.Name == "" {
			shared, ok := sharedParams[strings.TrimPrefix(param.Ref, "#/parameters/")]
			if !ok {
				continue
			}
			param = shared
		}
		if param.In == "body" {
			bodySchema = param.Schema
			break
		}
	}
	if bodySchema == nil {
		return nil
	}

	// The schema is a local $ref to a definition (e.g. "#/definitions/MsgVpn").
	if bodySchema.Ref != "" {
		def, ok := definitions[strings.TrimPrefix(bodySchema.Ref, "#/definitions/")]
		if !ok {
			return nil
		}
		bodySchema = def
	}
	if bodySchema.Value == nil || len(bodySchema.Value.Properties) == 0 {
		return nil
	}

	fields := make(map[string]bool, len(bodySchema.Value.Properties))
	for name := range bodySchema.Value.Properties {
		fields[name] = true
	}
	return fields
}
