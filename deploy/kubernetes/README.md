# Kubernetes deployment

Reference manifests for running the Solace Broker MCP Server in a cluster:
`configmap.yaml` (server config), `secret.yaml` (credentials),
`deployment.yaml` (the pod), and `service.yaml` (a ClusterIP Service).

They are a starting point to copy and edit, not a turnkey install. Applied
unmodified the pod will not start: `DEV_TOKEN` ships empty and the server
refuses to run without it. That is deliberate — see the table below.

## Edit these before applying

| Where | What | Why |
|---|---|---|
| `secret.yaml` | `DEV_TOKEN` | **Required.** Ships empty, so an unedited apply fails closed with `mcp_client_auth.dev_token is required`. Any caller that can reach the Service and presents this token gets full broker-admin-backed access — treat it like a password. A shipped default would be a credential published in this repository. |
| `secret.yaml` | `BROKER_PASSWORD` | Ships as `changeme`. |
| `secret.yaml` | `BROKER_USERNAME` | Defaults to `admin`. |
| `configmap.yaml` | `brokers.my-broker.url` | Points at `https://broker.example.com:943`. Until you change it, every tool call fails. |
| `deployment.yaml` | `image` tag | Ships as `:latest`. Pin a released version for a reproducible deploy. |

Do not commit an edited `secret.yaml`. For anything beyond a trial, use Vault,
Sealed Secrets, External Secrets Operator, or SOPS.

## Image tags

Container images are published to
`ghcr.io/solaceproducts/solace-broker-mcp`. **Image tags carry no `v`
prefix**, unlike the project's git tags — release `v0.8.0` publishes the image
tag `0.8.0`:

```
ghcr.io/solaceproducts/solace-broker-mcp:0.8.0    # correct
ghcr.io/solaceproducts/solace-broker-mcp:v0.8.0   # does not exist
```

Pulling the `v`-prefixed form fails with `manifest unknown`. Also published:
the major.minor alias (`0.8`), `latest`, and a `sha-<7hex>` tag per build.

## Apply

```bash
kubectl apply -f deploy/kubernetes/
kubectl rollout status deployment/solace-broker-mcp
```

Reach it from outside the cluster for a quick check:

```bash
kubectl port-forward svc/solace-broker-mcp 9090:9090
curl -s localhost:9090/readyz    # 200 once the pod is Ready
```

Health endpoints are unauthenticated; `/mcp` is not, and returns 401 without
the token.

## Client authentication

The ConfigMap defaults to `mcp_client_auth.mode: static` — a shared token from
the Secret — so the deployment runs with no identity provider to set up.

This is a development default. The pod must bind all interfaces for the Service
and the kubelet probes to reach it (`listen_address: "0.0.0.0"`), so the token
travels in cleartext to anything in the cluster that can route to the Service.
The server logs two `INSECURE MODE` warnings at startup saying exactly that.
Keep the Service `ClusterIP`, and treat the token as a real credential.

**For production, switch to OAuth.** `configmap.yaml` carries the `mode: oauth`
block as commented-out sample lines — uncomment it, delete the `mode: static`
and `dev_token` lines, and supply your identity provider's values. Two things
are easy to miss:

- `tool_authorization.enabled` must be set explicitly to `true` or `false`.
  The server refuses to start under `mode: oauth` without it.
- `tls_terminated_upstream: true` is required if the listener stays plaintext,
  because `mode: oauth` otherwise refuses to start. Set
  `tls_cert_file`/`tls_key_file` instead to terminate TLS in the pod.

See [`docs/authentication.md`](../../docs/authentication.md) for identity
provider setup and claim-based tool authorization.

## TLS

The Service exposes plaintext `:9090`. Terminate TLS at an Ingress or gateway
in front of it, and do not expose that port beyond the cluster.

## Health endpoints

| Path | Meaning |
|---|---|
| `/livez` | Liveness. Governs restarts. |
| `/readyz` | Readiness. Reflects the server's own state only — it makes no broker calls, so an unreachable broker does not make the pod unready. |

`deployment.yaml` wires a `startupProbe` that gates liveness and readiness
until cold-start init finishes (~60s budget).

## Shutdown

`terminationGracePeriodSeconds: 45` is sized deliberately: on SIGTERM the
server flips `/readyz` to 503, waits `obs.shutdown_drain_delay_s` (default 10s)
for the endpoint to be deregistered, then drains in-flight requests for up to
30s. The image is distroless with no shell, so this runs in-process and there
is no `preStop` hook. **If you raise the drain delay, raise this value to
match**, or Kubernetes will SIGKILL mid-drain.

## Configuration reference

`configmap.yaml` embeds the server's `config.yaml` under `data`. Every
available setting is documented in
[`docs/configuration.md`](../../docs/configuration.md), and
`broker-config.example.yaml` at the repository root is the annotated
standalone equivalent.
