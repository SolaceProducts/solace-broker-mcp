// Package definitions embeds the composite tool YAML files into the binary.
package definitions

import "embed"

// FS contains the embedded YAML tool definition files.
//
//go:embed *.yaml
var FS embed.FS
