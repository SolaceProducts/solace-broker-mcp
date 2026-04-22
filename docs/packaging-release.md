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
        ├─> build-binaries (matrix: 4 OS/arch combos, in parallel)
        ├─> build-docker (buildx multi-platform: linux/amd64, linux/arm64)
        └─────> release (collects all artifacts, creates GitHub Release)
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
| Helm chart | In-repo at `helm/solace-broker-mcp/` |

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
solace-broker-mcp                           # binary
broker-config.example.yaml                  # example configuration
LICENSE                                     # Apache 2.0
deploy/systemd/solace-broker-mcp.service    # systemd unit template
deploy/systemd/solace-broker-mcp.env.example # systemd environment file example
```

### Verifying checksums

```bash
sha256sum -c checksums-sha256.txt
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
  -e MY_BROKER_USERNAME=admin \
  -e MY_BROKER_PASSWORD=changeme \
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
      test: ["CMD-SHELL", "wget -qO- http://localhost:9090/health || exit 1"]
      interval: 15s
      timeout: 3s
      start_period: 5s
      retries: 3
```

Note: The Docker image uses distroless (no shell), so `wget` is not available
inside the container. The Docker Compose healthcheck above uses a sidecar-style
approach. For production, use an external health monitor or orchestrator probes.

### Kubernetes (Helm)

Install from the in-repo chart:

```bash
helm install my-release helm/solace-broker-mcp \
  --set config.brokers.prod.url=https://broker.example.com:943 \
  --set config.brokers.prod.auth.mode=basic \
  --set secret.brokerCredentials.PROD_USERNAME=admin \
  --set secret.brokerCredentials.PROD_PASSWORD=changeme
```

Or use a custom values file:

```bash
helm install my-release helm/solace-broker-mcp -f my-values.yaml
```

Example `my-values.yaml`:

```yaml
replicaCount: 2

config:
  developmentMode: false
  logLevel: info
  clientAuth:
    issuer: "https://idp.example.com"
    audience: "mcp-server"
    resourceUrl: "https://mcp.example.com/mcp"
  brokers:
    prod:
      url: "https://broker.example.com:943"
      auth:
        mode: basic
        username: "${PROD_USERNAME}"
        password: "${PROD_PASSWORD}"

secret:
  brokerCredentials:
    PROD_USERNAME: admin
    PROD_PASSWORD: changeme

resources:
  requests:
    cpu: 100m
    memory: 64Mi
  limits:
    cpu: 500m
    memory: 128Mi
```

The Helm chart configures:

- **Liveness probe**: `GET /health` on port 9090
- **Readiness probe**: `GET /health` on port 9090
- **Security context**: `runAsNonRoot: true`, `runAsUser: 65534`
- **Config**: mounted as `/etc/mcp-server/config.yaml` from a ConfigMap
- **Secrets**: injected as environment variables from a Kubernetes Secret

### systemd (Bare Metal / VM)

1. Create a dedicated user:
   ```bash
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin solace-broker-mcp
   ```

2. Install the binary:
   ```bash
   sudo cp solace-broker-mcp /usr/local/bin/
   sudo chmod 755 /usr/local/bin/solace-broker-mcp
   ```

3. Create the config directory and place the config file:
   ```bash
   sudo mkdir -p /etc/mcp-server
   sudo cp broker-config.example.yaml /etc/mcp-server/config.yaml
   sudo chown -R solace-broker-mcp:solace-broker-mcp /etc/mcp-server
   sudo chmod 640 /etc/mcp-server/config.yaml
   ```

4. Create the environment file with credentials:
   ```bash
   sudo cp deploy/systemd/solace-broker-mcp.env.example /etc/mcp-server/env
   sudo chmod 640 /etc/mcp-server/env
   # Edit /etc/mcp-server/env with actual credentials
   ```

5. Install the systemd service:
   ```bash
   sudo cp deploy/systemd/solace-broker-mcp.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now solace-broker-mcp
   ```

6. Verify:
   ```bash
   sudo systemctl status solace-broker-mcp
   curl http://localhost:9090/health
   ```

The systemd unit includes security hardening: `ProtectSystem=strict`,
`ProtectHome=true`, `NoNewPrivileges=true`, `Restart=on-failure` with a 5s
delay, and `TimeoutStopSec=125` (exceeds the server's 120s graceful shutdown
timeout).

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

| Deployment | Health check method |
|---|---|
| Docker | `curl http://localhost:9090/health` (external or Compose healthcheck) |
| Kubernetes | `httpGet` liveness/readiness probe on `/health` port 9090 |
| systemd | External monitor (e.g., `curl` in a timer or monitoring agent) |

Note: The Docker image uses distroless (no shell or HTTP client inside the
container). Health checks must be performed externally or via orchestrator
probes.

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
- **systemd unit is hardened** with `ProtectSystem=strict`,
  `NoNewPrivileges=true`, and `ProtectHome=true`.
- **Use Kubernetes Secrets** or an external secret manager (Vault, AWS Secrets
  Manager) for broker credentials. Avoid storing secrets in ConfigMaps or
  values files committed to source control.
- **Credentials are never logged.** The server redacts keys containing
  `password`, `token`, `secret`, `authorization`, `credential`, `api_key`,
  and `private_key` from all log output.
