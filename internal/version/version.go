// Package version holds the server version string, set at build time via
// ldflags. See docs/packaging-release.md for details.
package version

// Version is set at build time via:
//
//	go build -ldflags "-X github.com/SolaceDev/solace-broker-mcp/internal/version.Version=X.Y.Z" ./cmd/server
var Version = "dev"
