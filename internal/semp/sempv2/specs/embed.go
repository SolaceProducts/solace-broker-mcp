// Package specs embeds the Solace SEMPv2 OpenAPI specification files into the
// binary so they can be parsed at startup without external file dependencies.
//
// WARNING: These OpenAPI specs are currently committed to source.
// Future work: make these build-time artifacts fetched from the
// Solace documentation pipeline instead of source-committed files.
package specs

import "embed"

// FS contains the embedded OpenAPI JSON spec files (monitor, config, action).
//
//go:embed *.json
var FS embed.FS
