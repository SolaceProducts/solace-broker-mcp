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
needed (MCP server metadata, User-Agent header, health endpoints, and so on).

### Local development

Local builds use the fallback value. No action required — see
[Building > Local development binary](#local-development-binary).

### How ldflags works

ldflags is a **compile-time string substitution** mechanism. It does not fetch
anything from a remote repository. The long module path
(`github.com/SolaceProducts/solace-broker-mcp/internal/version.version`) is the
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
go build -ldflags "-X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=0.1.0" ./cmd/server
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
  -ldflags "-s -w -X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=${VERSION}" \
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
  includes CA certificates for TLS/OIDC, runs as non-root UID 65532
- **HEALTHCHECK**: uses the binary's `--health` flag (no shell or curl needed)

The `VERSION` build argument defaults to `dev` if not provided.

## Cutting a Release

The release flow — versioning and maturity model, gates, tagging, what the
workflow publishes, and rollback — lives in [`RELEASING.md`](../../RELEASING.md).
In short: push a SemVer `v*` tag and `.github/workflows/release.yml` does the
rest. No source files need to be edited to bump the version; the git tag is
the single source of truth.

## Release Artifacts

Each tagged release produces the following artifacts:

| Artifact | Location |
|---|---|
| `solace-broker-mcp-v0.1.0-linux-amd64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-linux-arm64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-darwin-amd64.tar.gz` | GitHub Release |
| `solace-broker-mcp-v0.1.0-darwin-arm64.tar.gz` | GitHub Release |
| `checksums-sha256.txt` | GitHub Release |
| Docker image | `ghcr.io/solaceproducts/solace-broker-mcp` |
| Example K8s manifests | In-repo at `deploy/kubernetes/` |

### Docker image tags

Image tags and moving pointers — including what `latest` does and does not
promise — are defined in
[`RELEASING.md` § Distribution pointers](../../RELEASING.md#distribution-pointers).

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
  ghcr.io/solaceproducts/solace-broker-mcp:latest
```

Verify the server is running:

```bash
curl http://localhost:9090/livez
# {"status":"alive"}
```

#### Docker Compose

```yaml
services:
  solace-broker-mcp:
    image: ghcr.io/solaceproducts/solace-broker-mcp:latest
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

[`deploy/kubernetes/README.md`](../../deploy/kubernetes/README.md) is the
source of truth for what must be edited before applying, the image-tag naming,
and the switch to production OAuth. In outline the manifests are:

- **`deployment.yaml`** — pod spec with config volume mount, secret env vars,
  startup/liveness/readiness probes (`httpGet /readyz` and `/livez`), security
  context (`runAsNonRoot: true`, `runAsUser: 65532`); defaults to `replicas: 2`
  with a pinned `maxUnavailable: 0` rollout strategy and best-effort topology
  spread across nodes
- **`service.yaml`** — ClusterIP service exposing port 9090, with
  `sessionAffinity: ClientIP` to pin a client to the pod holding its MCP
  session. Required above one replica, and bypassed entirely by an ingress or
  service mesh — see
  [Authentication](../authentication.md#session-routing-at-the-ingress-required-above-one-replica)
  § "Session Routing at the Ingress"
- **`ingress.yaml.example`** — copy-and-edit Ingress carrying the source-address
  hash annotation that replaces `sessionAffinity` once an ingress fronts the
  Service. The `.example` suffix keeps the directory-wide `kubectl apply` from
  picking it up. Rationale in
  [Authentication](../authentication.md#session-routing-at-the-ingress-required-above-one-replica)
- **`poddisruptionbudget.yaml`** — `maxUnavailable: 1`, so a node drain evicts
  one pod at a time. Applied automatically by the directory-wide `kubectl
  apply` above
- **`configmap.yaml`** — server configuration (broker URLs, SEMP settings).
  Note `semp.max_concurrent_per_broker` and `request_min_interval` are
  per-pod, so a broker sees `replicas ×` the configured value
- **`secret.yaml`** — broker credentials plus the MCP client dev token, as
  environment variables

Edit `configmap.yaml` with your broker URLs and settings. Edit `secret.yaml`
with your credentials. `DEV_TOKEN` backs the ConfigMap's default
`mcp_client_auth.mode: static` and ships empty on purpose — the server refuses
to start until it is set, so an unedited apply cannot come up on a credential
published in this repository:

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
  DEV_TOKEN: ""
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
manager (systemd, supervisord, launchd, and so on).

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

The server exposes a liveness probe at `GET /livez`, which returns:

```json
{"status":"alive"}
```

`GET /health` is retained for backward compatibility and preserves its
original `{"status":"healthy"}` body — it is NOT a body-identical alias of
`/livez`, so external consumers that parse `.status == "healthy"` keep working.
Both probes return HTTP 200 on success on the configured port (default 9090).

The binary also supports a `--health` flag that performs an internal health
check against the running server and exits with code 0 (healthy) or 1
(unhealthy). This is used for Docker HEALTHCHECK in the distroless image,
which has no shell, curl, or wget.

When the config file sets `tls_cert_file`/`tls_key_file`, the probe speaks HTTPS
and verifies the server's certificate against that same file, taking the
verification hostname from the certificate's first DNS or IP SAN (SOL-153167 —
it previously skipped verification, since the certificate rarely covers
`localhost`). Three consequences for a TLS-enabled image: the certificate must
be readable at the configured path from the probe's own process, it must carry a
DNS or IP SAN, and it must be within its validity dates. Any of those failing
reports the container unhealthy while the server keeps serving — which is also
what an in-place certificate rotation without a restart looks like, since the
server loads its keypair once at startup and would then be serving a superseded
certificate to every client anyway. The probe writes the reason to stderr, which
Docker records in `State.Health.Log`.

| Deployment | Health check method |
|---|---|
| Docker | `HEALTHCHECK CMD ["/solace-broker-mcp", "--health"]` (built into image) |
| Docker (external) | `curl http://localhost:9090/health` from host |
| Kubernetes | `httpGet` startup/readiness probe on `/readyz` and liveness probe on `/livez`, port 9090 |
| Bare metal | `curl http://localhost:9090/health` (cron, monitoring agent, and so on) |

## Security Considerations

- **Always set `mcp_client_auth.mode: oauth` in production.** A WARN-level boot
  banner fires for `disabled` and `static` modes (visible in startup logs),
  and those modes use the development profile (allowing `http://` URLs).
  Operators are expected to detect and prevent dev-mode configs in production
  via log alerting and deployment review.
- **Configure `mcp_client_auth.issuer`, `audience`, and `resource_url`** for
  production OAuth validation. The validator enforces `https://` on all
  three under `mode: oauth`.
- **Use `${VAR_NAME}` references** for credentials in the config file. Never
  hardcode passwords or tokens.
- **Enable TLS** via `tls_cert_file` and `tls_key_file`, or terminate TLS at
  a load balancer / ingress controller. Under `mode: oauth` the server refuses
  to start with a plaintext listener unless one of these is in place: set the
  certs, or set `tls_terminated_upstream: true` to acknowledge upstream
  termination (the server then serves plaintext and logs a startup WARN). Ensure
  the plaintext port's network scope is trusted — keep it behind the terminator.
- **Container runs as non-root** (UID 65532 in distroless). Do not override
  with `--user root`.
- **Run as a non-root user** on bare metal deployments. Create a dedicated
  service account rather than running as root.
- **Use Kubernetes Secrets** or an external secret manager (Vault, AWS Secrets
  Manager) for broker credentials. Avoid storing secrets in ConfigMaps or
  values files committed to source control.
- **Credentials are never logged.** The server redacts keys containing
  `password`, `token`, `secret`, `authorization`, `credential`, `api_key`,
  and `private_key` from all log output.
