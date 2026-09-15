# Kubernetes deployment

Reference manifests for running the Solace Broker MCP Server in a cluster:
`configmap.yaml` (server config), `secret.yaml` (credentials),
`deployment.yaml` (the pod), `service.yaml` (a ClusterIP Service),
`poddisruptionbudget.yaml` (keeps a pod serving through node drains), and
`networkpolicy.yaml` (admits `/metrics` scrapes from the monitoring namespace
only). Plus two `.example` files the directory-wide apply skips:
`ingress.yaml.example` — required reading above one replica; see
[TLS and ingress](#tls-and-ingress) — and `servicemonitor.yaml.example` for
Prometheus Operator; see [Metrics scraping](#metrics-scraping).

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
| `deployment.yaml` | `limits.memory` **and** `GOMEMLIMIT` | Only if you change either. They ship coupled at 512Mi / `384MiB` (75%) and must move together — see [Resource requests and limits](../../docs/observability.md#resource-requests-and-limits). Go's suffix is `MiB`, not Kubernetes' `Mi`; a value the runtime cannot parse is fatal at startup. |
| `networkpolicy.yaml` | `kubernetes.io/metadata.name: monitoring` | Only if Prometheus runs in another namespace. The policy admits `:9091` from that namespace and nothing else; a wrong name is a scrape that silently never happens, not an error. |
| `networkpolicy.yaml` | the `9090` and `9091` ports | Only if you change `port` in `configmap.yaml` or `observability.metrics_bind_address`. A port the policy does not list is denied outright: for `:9091` that is a scrape that stops, for `:9090` it is the MCP endpoint itself. Move the containerPorts in `deployment.yaml` and the `metrics` port in `service.yaml` with them. |
| `servicemonitor.yaml.example` | `release:` label | kube-prometheus-stack selects only ServiceMonitors carrying its own Helm release name. Wrong or missing, the target never appears and nothing logs it. |

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

### Pod stuck in `CrashLoopBackOff`?

`kubectl apply` reports success even when the server cannot start — the
resources are created, then the container exits. Startup problems are reported
in the pod log:

```bash
kubectl logs -l app.kubernetes.io/name=solace-broker-mcp --tail=20
```

The most common cause on a first deploy is an unset `DEV_TOKEN`, which reports:

```
failed to load config error="validating config:
  mcp_client_auth.dev_token is required when mcp_client_auth.mode is \"static\""
```

Every startup refusal names the offending configuration key, so the log line is
the fastest route to the cause.

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

## TLS and ingress

The Service exposes plaintext `:9090`. Terminate TLS at an Ingress or gateway
in front of it, and do not expose that port beyond the cluster.
`ingress.yaml.example` is a copy-and-edit starting point — it is named
`.example` so the directory-wide `kubectl apply` above skips it, since an
Ingress needs a hostname and TLS secret only you can supply.

> **Session routing is required, not optional.** An Ingress bypasses the
> Service's `sessionAffinity: ClientIP`, and with `replicas: 2` nearly every
> request after initialize then hits a pod that does not hold the session and
> gets `404 session not found`. `ingress.yaml.example` carries the annotation
> that fixes it (`upstream-hash-by: "$remote_addr"`). Neither cookie affinity
> nor hashing on `Mcp-Session-Id` works — see
> [`docs/authentication.md`](../../docs/authentication.md#session-routing-at-the-ingress-required-above-one-replica)
> § "Session Routing at the Ingress".

## Metrics scraping

`/metrics` is off by default and, when on, listens on a second port (`:9091`)
that the Service exposes as `metrics`. Three shipped pieces make it scrapeable
from day one:

1. **Turn the listener on.** Uncomment `OBS_METRICS_ENABLED` in
   `deployment.yaml`. Until then the port resolves to `connection refused`.
2. **Restrict who can reach it.** `networkpolicy.yaml` applies with the
   directory and admits `:9091` only from the `monitoring` namespace. `:9090`
   stays open to every source, because the kubelet's probes arrive from the
   node and no selector can name them. It restricts ingress only; broker, IdP,
   and OTLP egress are untouched. It needs a CNI that enforces NetworkPolicy
   (Calico, Cilium, GKE/AKS/EKS native) — kind's default CNI accepts it and
   enforces nothing, so verify with a `curl` from a pod outside `monitoring`.
3. **Point Prometheus at it.** `servicemonitor.yaml.example` is the Prometheus
   Operator manifest: copy it, edit the `release:` label, apply it to the same
   namespace as the Service. It is `.example` because the CRD is not on every
   cluster and one missing kind fails the whole directory apply.

The two ways a correct ServiceMonitor is silently never scraped, the annotation
fallback for clusters without Prometheus Operator, what to do when your CNI does
not enforce NetworkPolicy, and where the OTLP egress rule belongs if your cluster
default-denies egress are all in
[Scraping and securing the metrics endpoint](../../docs/observability.md#scraping-and-securing-the-metrics-endpoint).

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
for the endpoint to be deregistered, drains in-flight requests for up to 30s,
then flushes any registered shutdown hooks (OTel providers) for up to 3s more.
The image is distroless with no shell, so this runs in-process and there is no
`preStop` hook. **If you raise the drain delay, raise this value to match**,
or Kubernetes will SIGKILL mid-drain.

## Configuration reference

`configmap.yaml` embeds the server's `config.yaml` under `data`. Every
available setting is documented in
[`docs/configuration.md`](../../docs/configuration.md), and
`broker-config.example.yaml` at the repository root is the annotated
standalone equivalent.
