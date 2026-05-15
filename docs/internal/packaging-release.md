# Packaging, Release & Deployment

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

Local builds use the fallback value. No action required — see
[Building > Local development binary](#local-development-binary).

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

## Building

### Local development binary

```bash
go build ./cmd/server
# Produces ./server with version "dev"
```

### Production binary

Strip debug symbols with `-s -w` for a smaller binary and inject the version:

```bash
VERSION=$(git describe --tags --always)
CGO_ENABLED=0 go build \
  -ldflags "-s -w -X github.com/SolaceDev/solace-broker-mcp/internal/version.version=${VERSION}" \
  -o solace-broker-mcp \
  ./cmd/server
```

`CGO_ENABLED=0` produces a fully static binary suitable for minimal container
images (distroless, scratch) and cross-compilation.

### Docker image

```bash
docker build --build-arg VERSION=0.1.0 -t solace-broker-mcp:0.1.0 .
```

The Dockerfile uses a multi-stage build:

- **Builder stage** (`golang:1.25-alpine`): compiles the binary with ldflags
- **Runtime stage** (`gcr.io/distroless/static-debian12:nonroot`): ~2 MB base,
  includes CA certificates for TLS/OIDC, runs as non-root UID 65534
- **HEALTHCHECK**: uses the binary's `--health` flag (no shell or curl needed)

The `VERSION` build argument defaults to `dev` if not provided.

## Cutting a Release

1. Merge all changes to `main`.
2. Create an annotated tag:
   ```bash
   git tag -a v0.1.0 -m "Release v0.1.0"
   ```
3. Push the tag:
   ```bash
   git push origin v0.1.0
   ```
4. The release workflow (`.github/workflows/release.yml`) runs automatically:
   - Runs the full test suite (lint, unit tests, e2e tests)
   - Cross-compiles binaries for 4 platforms
   - Packages each binary into a `.tar.gz` archive
   - Builds and pushes multi-platform Docker images to ghcr.io
   - Generates SHA256 checksums
   - Creates a GitHub Release with all assets and auto-generated release notes

No source files need to be edited to bump the version. The git tag is the
single source of truth.

### Release workflow pipeline

```
push v* tag
  └─> test (reuses build-and-test.yml)
        ├─> build-binaries (matrix: 4 OS/arch)  ──┐
        └─> build-docker (buildx multi-platform) ─┴─> release (GitHub Release)
```

## Release Artifacts

Each tagged release produces the following artifacts:

| Artifact | Location |
|---|---|
| `solace-broker-mcp-v0.1.0-linux-amd64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-linux-arm64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-darwin-amd64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-darwin-arm64.tar.gz` | GitHub Release |
| `checksums-sha256.txt` | GitHub Release |
| Docker image | `ghcr.io/solacedev/solace-broker-mcp` |
| Example K8s manifests | In-repo at `deploy/kubernetes/` |

### Docker image tags

| Tag | Example | Description |
|---|---|---|
| semver | `0.1.0` | Full version (without `v` prefix) |
| major.minor | `0.1` | Tracks latest patch for a minor version |
| sha | `sha-abcdef1` | Immutable, tied to exact commit |
| `latest` | `latest` | Most recent release |

### Archive contents

Each `.tar.gz` contains:

```
solace-broker-mcp          # binary
broker-config.example.yaml # example configuration
LICENSE                    # Apache 2.0
```

## Deployment

### Docker

Run the container with a config file mounted and credentials passed via
environment variables:

```bash
docker run -d \
  --name solace-broker-mcp \
  -p 9090:9090 \
  -v /path/to/config.yaml:/etc/mcp-server/config.yaml:ro \
  -e BROKER_USERNAME=admin \
  -e BROKER_PASSWORD=changeme \
  ghcr.io/solacedev/solace-broker-mcp:latest
```

Verify the server is running:

```bash
curl http://localhost:9090/health
# {"status": "ok"}
```

#### Docker Compose

```yaml
services:
  solace-broker-mcp:
    image: ghcr.io/solacedev/solace-broker-mcp:latest
    ports:
      - "9090:9090"
    volumes:
      - ./config.yaml:/etc/mcp-server/config.yaml:ro
    env_file:
      - .env
    healthcheck:
      test: ["CMD", "/solace-broker-mcp", "--health"]
      interval: 15s
      timeout: 3s
      start_period: 5s
      retries: 3
```

### Kubernetes

Example manifests are provided in `deploy/kubernetes/`. Copy them, edit for
your environment, and apply:

```bash
kubectl apply -f deploy/kubernetes/
```

The manifests include:

- **`deployment.yaml`** — pod spec with config volume mount, secret env vars,
  liveness/readiness probes (`httpGet /health`), security context
  (`runAsNonRoot: true`, `runAsUser: 65534`)
- **`service.yaml`** — ClusterIP service exposing port 9090
- **`configmap.yaml`** — server configuration (broker URLs, SEMP settings)
- **`secret.yaml`** — broker credentials as environment variables

Edit `configmap.yaml` with your broker URLs and settings. Edit `secret.yaml`
with your credentials:

```yaml
# WARNING: Do not commit this file with real credentials.
# Use a secret manager (Vault, sealed-secrets, External Secrets) in production.
apiVersion: v1
kind: Secret
metadata:
  name: solace-broker-mcp
stringData:
  BROKER_USERNAME: admin
  BROKER_PASSWORD: changeme
```

> **Future:** A Helm chart for templated deployments, rollback, and
> multi-instance management is planned. See `docs/todo-gaps.md` for details.

### Bare Metal / VM

The binary is statically linked with no external dependencies.

1. Extract the release archive:
   ```bash
   tar xzf solace-broker-mcp-v0.1.0-linux-amd64.tar.gz
   ```

2. Place the config file:
   ```bash
   sudo mkdir -p /etc/mcp-server
   sudo cp broker-config.example.yaml /etc/mcp-server/config.yaml
   # Edit /etc/mcp-server/config.yaml with broker URLs and auth settings
   ```

3. Set credentials and run:
   ```bash
   export BROKER_USERNAME=admin
   export BROKER_PASSWORD=changeme
   CONFIG_FILE=/etc/mcp-server/config.yaml ./solace-broker-mcp
   ```

The server handles `SIGTERM` and `SIGINT` for graceful shutdown (120s timeout).
To run as a background service with auto-restart, use your platform's process
manager (systemd, supervisord, launchd, etc.).

> **Future:** Pre-built service templates (systemd unit, launchd plist) are
> planned for a future release. See `docs/todo-gaps.md` for details.

## Configuration

### Config file resolution

The server searches for configuration in this order:

| Priority | Source | Description |
|---|---|---|
| 1 | `CONFIG_FILE` env var | Explicit path; fatal if set but unreadable |
| 2 | `/etc/mcp-server/config.yaml` | System install path (Docker, K8s, systemd) |
| 3 | `./broker-config.yaml` | Developer convenience (local repo) |

### Key environment variables

| Variable | Description | Default |
|---|---|---|
| `CONFIG_FILE` | Override config file path | (search order above) |
| `ENV_FILE` | Override `.env` file path | `.env` next to config file |
| `MCP_SERVER_PORT` | Override HTTP listen port | `9090` |

### Credential substitution

Use `${VAR_NAME}` in any YAML field to reference environment variables. The
server substitutes these before parsing the YAML:

```yaml
brokers:
  prod:
    auth:
      mode: basic
      username: "${PROD_USERNAME}"
      password: "${PROD_PASSWORD}"
```

Variables are loaded from the `.env` file (next to the config file, or at the
path specified by `ENV_FILE`) and from the process environment.

## Health Checks

The server exposes `GET /health` which returns:

```json
{"status": "ok"}
```

HTTP 200 on success, available on the configured port (default 9090).

The binary also supports a `--health` flag that performs an internal health
check against the running server and exits with code 0 (healthy) or 1
(unhealthy). This is used for Docker HEALTHCHECK in the distroless image,
which has no shell, curl, or wget.

| Deployment | Health check method |
|---|---|
| Docker | `HEALTHCHECK CMD ["/solace-broker-mcp", "--health"]` (built into image) |
| Docker (external) | `curl http://localhost:9090/health` from host |
| Kubernetes | `httpGet` liveness/readiness probe on `/health` port 9090 |
| Bare metal | `curl http://localhost:9090/health` (cron, monitoring agent, etc.) |

## Security Considerations

- **Always set `development_mode: false` in production.** Development mode
  disables OAuth token validation and uses a static dev token.
- **Configure `client_auth`** with a proper OIDC issuer, audience, and resource
  URL for production OAuth validation.
- **Use `${VAR_NAME}` references** for credentials in the config file. Never
  hardcode passwords or tokens.
- **Enable TLS** via `tls_cert_file` and `tls_key_file`, or terminate TLS at
  a load balancer / ingress controller.
- **Container runs as non-root** (UID 65534 in distroless). Do not override
  with `--user root`.
- **Run as a non-root user** on bare metal deployments. Create a dedicated
  service account rather than running as root.
- **Use Kubernetes Secrets** or an external secret manager (Vault, AWS Secrets
  Manager) for broker credentials. Avoid storing secrets in ConfigMaps or
  values files committed to source control.
- **Credentials are never logged.** The server redacts keys containing
  `password`, `token`, `secret`, `authorization`, `credential`, `api_key`,
  and `private_key` from all log output.
