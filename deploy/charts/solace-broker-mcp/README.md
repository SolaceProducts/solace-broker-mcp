# solace-broker-mcp Helm chart

Deploys the Solace Broker MCP server into Kubernetes: a Deployment, a
Service, a ConfigMap holding the server config, and a Secret holding broker
credentials and the MCP dev token.

The chart deploys **only the MCP server**. It does not deploy Prometheus,
Grafana, or an OpenTelemetry collector; use their own charts.

> **Status: `0.x`, parity in progress.** The chart does not yet cover
> everything in [`deploy/kubernetes/`](../../kubernetes/): ServiceMonitor,
> NetworkPolicy, PodDisruptionBudget, Ingress, observability switches, and
> OAuth client authentication are still to come. Until the chart is published
> to a registry, install it from a checkout of this repository.

## Install

You need Helm 3 and, for each broker, its SEMP URL and credentials.

1. Write a values file, for example `my-values.yaml`:

   ```yaml
   brokers:
     my-broker:                       # broker name, as tools see it
       url: "https://broker.example.com:943"
       auth:
         mode: basic                  # basic | bearer
         username: admin
         password: "<SEMP password>"

   mcpClientAuth:
     devToken: "<a long random string>"
   ```

   For anything beyond a trial, keep credentials out of values files and use
   [existing Secrets](#credentials-and-secrets) instead.

2. Install from the repository root:

   ```bash
   helm install mcp deploy/charts/solace-broker-mcp -f my-values.yaml
   ```

3. Check that the server is ready:

   ```bash
   kubectl rollout status deployment/mcp-solace-broker-mcp
   kubectl port-forward service/mcp-solace-broker-mcp 9090:9090
   curl -s http://127.0.0.1:9090/readyz
   ```

   MCP clients connect to `http://<service>:9090/mcp` and send the dev token
   as a Bearer token.

**The default values do not render.** The server refuses to start without a
broker and a dev token, so the chart fails `helm install` instead of starting a
pod that would crash-loop. The same applies to an empty broker `url`, an
unsupported `auth.mode`, and a chart-managed credential left empty.

The dev token is a single shared credential, sent in cleartext to anything in
the cluster that can reach the Service. Keep the Service `ClusterIP`.

## Upgrade

```bash
helm upgrade mcp deploy/charts/solace-broker-mcp -f my-values.yaml
```

The pod template carries checksums of the rendered config and of the
chart-managed Secret, so a change to either rolls the pods. The rollout keeps
capacity (`maxUnavailable: 0`, `maxSurge: 1`).

The Secret checksum is a hash of the chart-managed credentials, and anyone who
can read the Deployment can read it. A weak password or dev token can be
guessed offline against it. Use long random credentials, or supply them through
existing Secrets, which the checksum does not cover.

A change to an **existing** Secret you manage yourself does not roll the pods.
Restart them after rotating it:

```bash
kubectl rollout restart deployment/mcp-solace-broker-mcp
```

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/solaceproducts/solace-broker-mcp` | Server image |
| `image.tag` | `""` | Defaults to the chart's `appVersion`: a release version without the leading `v` |
| `image.pullPolicy` | `IfNotPresent` | |
| `replicaCount` | `2` | Above one replica, clients are pinned to a pod by the Service's `sessionAffinity: ClientIP` (MCP sessions live in one pod's memory) |
| `brokers` | `{}` | **Required.** Broker name → connection; see [Brokers](#brokers) |
| `mcpClientAuth.mode` | `static` | Only `static` is supported in this chart version |
| `mcpClientAuth.devToken` | `""` | Chart-managed dev token. Ignored when `existingSecret` is set |
| `mcpClientAuth.existingSecret` | `""` | Secret holding the dev token under key `dev-token` |
| `server.port` | `9090` | Server and Service port. Must not be `9091`, the metrics port |
| `server.logLevel` | `info` | `debug`, `info`, `warn` or `error` |
| `server.listenAddress` | `"0.0.0.0"` | `"0.0.0.0"` or `"::"` only: a pod must bind all interfaces for the Service and probes to reach it, and its IP is not known at render time |
| `semp.*` | as in `deploy/kubernetes/configmap.yaml` | SEMP client tuning: `maxConcurrentPerBroker`, `requestTimeoutDuration`, `requestMinInterval`, `maxQueueWait`, `fairScheduling`, `retries`, `retryMinInterval`, `retryMaxInterval`. Durations are strings such as `"0s"`; the server rejects a bare number. See `docs/configuration.md` |
| `resources` | requests `cpu: 100m`, `memory: 128Mi`; limits `memory: 512Mi` | No CPU limit by design: CFS throttling stalls the bursts this workload makes |
| `goMemLimit` | `""` | `GOMEMLIMIT` override. Empty derives 75% of `resources.limits.memory`, floored to whole MiB (`512Mi` → `384MiB`, `1Gi` → `768MiB`). Only integer `Mi` and `Gi` limits can be derived; for any other form set this explicitly, in Go's form (`MiB`, not `Mi`), or `off` |

Server settings follow one rule: a key set to `null` is left out of the
rendered config, so the server's own default applies. For example,
`--set semp.retries=null`. The exception is `server.listenAddress`, which the
chart requires: the server's default there is loopback.

### Brokers

Each key under `brokers` is a broker name, as the server's tools see it
(letters, digits and `-`, compared case-insensitively):

```yaml
brokers:
  my-broker:
    url: "https://broker.example.com:943"   # required
    insecureSkipVerify: false               # optional; dev only
    envPrefix: ""                           # default: name upper-cased, "-" -> "_"
    auth:
      mode: basic                           # basic | bearer
      username: ""                          # chart-managed (ignored when existingSecret is set)
      password: ""
      token: ""                             # bearer
      existingSecret: ""                    # keys: username + password (basic) or token (bearer)
```

Because Helm merges maps, the default `brokers` is empty: your values file
supplies every broker.

## Credentials and Secrets

Each credential reaches the pod as one env var, which the rendered config
references as `${...}`. Credentials are never written into the ConfigMap.

| Credential | Env var | Key in an existing Secret |
|---|---|---|
| Broker username (`basic`) | `<PREFIX>_USERNAME` | `username` |
| Broker password (`basic`) | `<PREFIX>_PASSWORD` | `password` |
| Broker token (`bearer`) | `<PREFIX>_TOKEN` | `token` |
| MCP dev token | `DEV_TOKEN` | `dev-token` |

`<PREFIX>` is the broker's `envPrefix`, by default its name upper-cased with
`-` replaced by `_` (`my-broker` → `MY_BROKER`). A custom `envPrefix` must
match `^[A-Z_][A-Z0-9_]*$`, since the server substitutes only upper-case
`${...}` references; a broker name starting with a digit therefore needs one.
Setting `envPrefix: BROKER` on a single broker reproduces the env var names in
`deploy/kubernetes/secret.yaml`.

- **Chart-managed:** credentials set in values go into one Secret named after
  the release (`<release>-solace-broker-mcp`), loaded with `envFrom`. It is
  created only when at least one credential is chart-managed.
- **Existing Secret:** set `brokers.<name>.auth.existingSecret` or
  `mcpClientAuth.existingSecret`. The chart injects each key with
  `secretKeyRef` under the same env var name. Create the Secret first:

  ```bash
  kubectl create secret generic prod-semp \
    --from-literal=username=admin --from-literal=password='<SEMP password>'
  kubectl create secret generic mcp-dev-token \
    --from-literal=dev-token='<a long random string>'
  ```

## Pod security

The pod runs as UID 65532 (distroless `nonroot`) with a `RuntimeDefault`
seccomp profile, no service account token, no privilege escalation, all
capabilities dropped, and a read-only root filesystem. `enableServiceLinks` is
off: Kubernetes would otherwise inject `<SERVICE>_PORT`-style variables for
every Service in the namespace, and a Service named `mcp-server` produces
`MCP_SERVER_PORT`, which the server reads as its own port override.

## Developing the chart

Before pushing a chart change, run from the repository root:

```bash
make helm-check
```

It runs the same steps as CI's `helm-chart` job: `ct lint` with every
`ci/*-values.yaml`, a chart version bump check against `main`, `kubeconform`
over the rendered chart, and the helm-unittest suites in `tests/`. Bump
`version` in `Chart.yaml` in every pull request that changes the chart.

Tools, at the versions CI pins:

| Tool | Version |
|---|---|
| [Helm](https://helm.sh/docs/intro/install/) | v3.22.x |
| [helm-unittest](https://github.com/helm-unittest/helm-unittest) plugin | v1.2.x (pinned with Helm v3.22: helm-unittest v1.1+ uses a plugin field Helm 3.17 rejects) |
| [chart-testing](https://github.com/helm/chart-testing) (`ct`) | v3.15.0, with its `etc/` files in `~/.ct` |
| `yamllint` and `yamale` (Python, used by `ct lint`) | 1.38.0 and 6.1.0 |
| [kubeconform](https://github.com/yannh/kubeconform) | v0.6.7 |
