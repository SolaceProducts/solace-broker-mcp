# Packaging & Release

## Versioning

The MCP server version is managed via Go **ldflags build-time injection**. The
source code contains a fallback default; the real version is injected during the
build.

### Source

```go
// internal/version/version.go
package version

var Version = "dev"
```

`Version` is a package-level variable imported wherever the version string is
needed (MCP server metadata, User-Agent header, health endpoints, etc.).

### Local development

Local builds use the fallback value. No action required:

```bash
go build ./cmd/server
# Version = "dev"
```

### How ldflags works

ldflags is a **compile-time string substitution** mechanism. It does not fetch
anything from a remote repository. The long module path
(`github.com/SolaceDev/solace-broker-mcp/internal/version.version`) is the
same path used in Go `import` statements — the compiler resolves it against
whatever source is on disk and replaces the variable's initial value in the
compiled binary.

The version value itself comes from whatever string you pass. Common sources:

| Source | Command | Example output |
|---|---|---|
| Git tag | `git describe --tags --always` | `v0.1.0` or `v0.1.0-3-gabcdef` |
| CI variable | `$CI_TAG` or `$GITHUB_REF_NAME` | `v0.2.0` |
| Manual | Hardcoded in build script | `0.1.0` |

### Injecting a version at build time

Pass the fully-qualified variable path via `-ldflags -X`:

```bash
go build -ldflags "-X github.com/SolaceDev/solace-broker-mcp/internal/version.version=0.1.0" ./cmd/server
```

### CI / release builds

Use a git tag as the version source so the binary version always matches the
repository tag:

```bash
VERSION=$(git describe --tags --always)
go build -ldflags "-X github.com/SolaceDev/solace-broker-mcp/internal/version.version=${VERSION}" ./cmd/server
```

### Cutting a new release

1. Merge all changes to the release branch.
2. Create an annotated tag: `git tag -a v0.2.0 -m "Release v0.2.0"`
3. Push the tag: `git push origin v0.2.0`
4. CI picks up the tag and injects it via ldflags into the build.

No source files need to be edited to bump the version.
