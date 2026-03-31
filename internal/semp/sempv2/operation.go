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
	ID          string      // operationId from OpenAPI spec (e.g., "getMsgVpnQueue")
	Method      string      // HTTP method (GET, POST, PUT, PATCH, DELETE)
	Path        string      // path template including basePath (e.g., "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}")
	Parameters  []Parameter // path, query, and body parameters
	Description string      // human-readable description from the spec
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
	"monitor": true,
	"config":  true,
	"action":  true,
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
				}
			}
		}
	}

	if len(operations) == 0 {
		return nil, fmt.Errorf("no operations found in spec files")
	}

	return operations, nil
}

// deriveSpecType extracts the SEMP API type from a spec's basePath.
// For example, "/SEMP/v2/monitor" returns "monitor". Returns an error if the
// basePath does not end with a recognized type (monitor, config, action).
func deriveSpecType(basePath string) (string, error) {
	parts := strings.Split(strings.TrimSuffix(basePath, "/"), "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("empty basePath")
	}
	specType := parts[len(parts)-1]
	if !validSpecTypes[specType] {
		return "", fmt.Errorf("unrecognized spec type %q from basePath %q (expected monitor, config, or action)", specType, basePath)
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
