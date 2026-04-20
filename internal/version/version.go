// Package version holds the server version string, set at build time via
// ldflags. See docs/packaging-release.md for details.
package version

// version is set at build time via:
//
//	go build -ldflags "-X github.com/SolaceDev/solace-broker-mcp/internal/version.version=X.Y.Z" ./cmd/server
var version = "dev"

// Version returns the server version string.
func Version() string { return version }
