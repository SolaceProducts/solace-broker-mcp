# solace-broker-mcp Helm chart

Deploys the Solace Broker MCP server into Kubernetes: a Deployment, a
Service, a ConfigMap holding the server config, and a Secret holding broker
credentials and, under `mcpClientAuth.mode: static`, the MCP dev token. A ServiceMonitor, NetworkPolicy,
PodDisruptionBudget and Ingress are optional, each off by default.

The chart deploys **only the MCP server**. It does not deploy Prometheus,
Grafana, or an OpenTelemetry collector; use their own charts
(`kube-prometheus-stack`, `grafana/grafana`,
`open-telemetry/opentelemetry-collector`) and point this chart at them. See
[Observability](#observability).

> **At parity with [`deploy/kubernetes/`](../../kubernetes/).** Every
> resource and setting the raw manifests carry, including their `.example`
> files and their commented-out alternatives (OAuth, the `OBS_*` switches, the
> `observability:` tunables), can be produced from this chart's values. Not
> exposed as values: the edits the raw files suggest only in prose comments
> (spreading replicas across zones with `topologyKey`, narrowing the
> NetworkPolicy's metrics rule with a `podSelector`, dropping the Ingress's
> `/.well-known` path under static), and in-pod TLS, which the raw ConfigMap
> mentions as an alternative to `tls_terminated_upstream`. The
> `helm-chart` CI job keeps the two in step: a change to either side alone
> fails it ([Developing the chart](#developing-the-chart)). Beyond what the raw
> manifests carry, the chart does not yet cover settings that need a mounted
> file (in-pod TLS, CA bundles; SOL-156417) or broker-side OAuth (SOL-156418).
> Until the chart is published to a registry, install it from a checkout of
> this repository.

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

A broker `url` is stored in the ConfigMap in plain text, so it must not hold
any secret. The chart rejects credentials before the host (`user:pass@`) and
any query string or fragment, but it cannot detect a secret in the path. Put
credentials in the broker's `auth` block or `auth.existingSecret`.

The dev token is a single shared credential, sent in cleartext to anything in
the cluster that can reach the Service. Keep the Service `ClusterIP`.

## Upgrade

```bash
helm upgrade mcp deploy/charts/solace-broker-mcp -f my-values.yaml
```

Pass your values file on every upgrade; do not use `--reuse-values`. With it,
Helm reuses the previous release's values in place of the new chart's
defaults, so defaults a newer chart version adds never load: enabling the
Ingress that way after upgrading from `0.1.0` renders it with no
`ingressClassName` and none of the session-affinity annotations, and sessions
404 above one replica. `--reset-then-reuse-values` applies the new defaults.

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
| `image.pullPolicy` | `IfNotPresent` | `null` omits `imagePullPolicy`, so Kubernetes' default applies (`Always` for `:latest`) |
| `replicaCount` | `2` | Above one replica, clients are pinned to a pod by the Service's `sessionAffinity: ClientIP` (MCP sessions live in one pod's memory) |
| `brokers` | `{}` | **Required.** Broker name → connection; see [Brokers](#brokers) |
| `mcpClientAuth.mode` | `static` | `static` (shared dev token) or `oauth`; see [Client authentication](#client-authentication) |
| `mcpClientAuth.devToken` | `""` | `static` only. Chart-managed dev token. Ignored when `existingSecret` is set, and under `oauth` |
| `mcpClientAuth.existingSecret` | `""` | `static` only. Secret holding the dev token under key `dev-token` |
| `mcpClientAuth.issuer`, `.audience`, `.resourceUrl` | `null` | **Required** under `oauth`; `issuer` and `resourceUrl` as `https://` URLs |
| `mcpClientAuth.scopesSupported` | `null` | Scopes advertised to MCP clients; `null` = `["openid"]`. Entra needs `offline_access` |
| `mcpClientAuth.toolAuthorization.*` | `null`, `accessLevelGroups: {}` | `oauth` only: `enabled` (required under `oauth`), `groupsClaimName`, `filterToolsList`, `accessLevelGroups`; see [Tool authorization](#tool-authorization) |
| `enableWriteTools` | `null` | `true` registers the write/action tools; `null` = the server default, `false` |
| `tlsTerminatedUpstream` | `null` | **Required `true`** under `oauth`: acknowledges TLS is terminated at the Ingress or gateway |
| `allowInsecureBrokerTls` | `null` | Under `oauth`, accepts a broker with `insecureSkipVerify: true`; without it such a broker fails rendering |
| `server.port` | `9090` | Server and Service port. Must differ from the metrics port (`9091`, or the port in `observability.metrics.bindAddress`) |
| `server.logLevel` | `info` | `debug`, `info`, `warn` or `error` |
| `server.listenAddress` | `"0.0.0.0"` | `"0.0.0.0"` or `"::"` only: a pod must bind all interfaces for the Service and probes to reach it, and its IP is not known at render time |
| `semp.*` | as in `deploy/kubernetes/configmap.yaml` | SEMP client tuning: `maxConcurrentPerBroker`, `requestTimeoutDuration`, `requestMinInterval`, `maxQueueWait`, `fairScheduling`, `retries`, `retryMinInterval`, `retryMaxInterval`. Durations are strings such as `"0s"`; the server rejects a bare number. See `docs/configuration.md` |
| `resources` | requests `cpu: 100m`, `memory: 128Mi`; limits `memory: 512Mi` | No CPU limit by design: CFS throttling stalls the bursts this workload makes |
| `observability.*` | all off except correlation IDs | Observability switches and the OTLP endpoint; see [Observability](#observability) |
| `observability.shutdownDrainDelayS` | `null` | Drain delay after SIGTERM (server default 10s). `terminationGracePeriodSeconds` follows it: this + 35, else `45` |
| `observability.serviceName`, `.serviceInstanceId`, `.deploymentEnvironment`, `.cloudRegion` | `null` | Telemetry identity. The server also reads `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`, which [`extraEnv`](#extra-environment-variables) can set |
| `observability.saturationThresholdMs`, `.otelSelfStatsIntervalS` | `null` | Saturation log threshold (server default 1000) and self-stats interval (server default 60) |
| `networkPolicy.enabled` | `false` | Renders a NetworkPolicy; see [NetworkPolicy](#networkpolicy) |
| `networkPolicy.metricsNamespace` | `monitoring` | The namespace allowed to reach the metrics port |
| `podDisruptionBudget.enabled` | `false` | Renders a PodDisruptionBudget with `maxUnavailable: 1` |
| `ingress.enabled` | `false` | Renders an Ingress; see [Ingress](#ingress) |
| `ingress.className` | `nginx` | `ingressClassName`; `""` omits it, so the cluster's default class applies |
| `ingress.host` | `""` | **Required** when the Ingress is enabled |
| `ingress.tls.secretName` | `""` | TLS Secret for `ingress.host`; empty renders no `tls` block |
| `ingress.annotations` | the four ingress-nginx annotations | Set a key to `null` to remove it |
| `extraEnv` | `[]` | Extra container env vars as full `EnvVar` entries (`value` or `valueFrom`); see [Extra environment variables](#extra-environment-variables) |
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

## Client authentication

`mcpClientAuth.mode` selects how MCP clients authenticate
([`docs/authentication.md`](../../../docs/authentication.md)):

- **`static`** (default): one shared dev token, so the chart installs with no
  identity provider. Keep the Service `ClusterIP`.
- **`oauth`**: clients present a JWT from your identity provider. It is the
  only mode meant to be reachable from outside the cluster, and the only one
  with per-caller identity in the audit log.

```yaml
mcpClientAuth:
  mode: oauth
  issuer: "https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0"
  audience: "11111111-1111-1111-1111-111111111111"
  resourceUrl: "https://mcp.example.com/mcp"
  # Microsoft Entra ID: keep openid, add offline_access (or clients get no
  # refresh token to renew with) and the delegated scope exposed on this
  # server's app registration. For Keycloak, omit the key: ["openid"] is enough.
  scopesSupported:
    - openid
    - offline_access
    - "https://mcp.example.com/mcp/access_as_user"
  toolAuthorization:
    enabled: false        # required under oauth; see Tool authorization
# TLS terminates at the Ingress: oauth refuses a plaintext listener otherwise.
tlsTerminatedUpstream: true
ingress:
  enabled: true
  host: mcp.example.com   # resourceUrl's host (not checked by the chart)
  tls:
    secretName: mcp-example-tls
```

The chart fails rendering on these oauth setups, which the server would
refuse at startup: a missing `issuer`, `audience` or `resourceUrl`; an
`issuer`, `resourceUrl` or broker `url` that is not `https://` (or, for the
first two, that the server cannot parse as a URL; a value carrying a
`${VAR_NAME}` reference, which the server substitutes first, skips that parse
check, and the `https://` one too when it starts with the reference); `tlsTerminatedUpstream`
not `true` (this chart cannot yet terminate TLS in the pod, SOL-156417); a
broker with `insecureSkipVerify: true` unless `allowInsecureBrokerTls: true`;
`toolAuthorization.enabled` left unset; and a blank group name, or a tool name
that is empty or has surrounding whitespace, in `accessLevelGroups`. Whether a
granted tool exists is checked only by the server, at startup: a misspelled
tool name renders, and with `enabled: true` the pod then fails to start. Under `oauth` the dev token is not
used: `devToken` and `mcpClientAuth.existingSecret` are ignored, so a shared
base values file can keep them.

### Tool authorization

`mcpClientAuth.toolAuthorization` gates each tool by the groups in the
caller's token
([`docs/authentication.md`](../../../docs/authentication.md#tool-authorization-claim-based-rbac)).
It is `oauth` only: setting any of its keys under `static` fails rendering.

```yaml
mcpClientAuth:
  toolAuthorization:
    enabled: true
    groupsClaimName: roles      # null = "groups"
    filterToolsList: true       # tools/list shows each caller only what it may call
    accessLevelGroups:          # required when enabled is true
      mcp-readers: [list-queues, list-vpns]
      mcp-operators: [create-queue]
```

`enabled: false` lets any authenticated caller invoke any registered tool.
`accessLevelGroups` defaults to empty because Helm merges maps: your values
file supplies every group, and setting a group to `null` drops one that a
base values file granted.

### Write tools

The write/action tools (create, update and delete broker configuration, plus
operational actions) are registered only with `enableWriteTools: true`
([`docs/user-guide.md`](../../../docs/user-guide.md)). It is independent of
the client authentication mode, and it is the only control that removes them
for everyone: a group in `accessLevelGroups` that grants a write tool while
`enableWriteTools` is off renders, as the server only warns about it, but the
tool is never registered.

## Credentials and Secrets

Each credential reaches the pod as one env var, which the rendered config
references as `${...}`. Credentials are never written into the ConfigMap.

| Credential | Env var | Key in an existing Secret |
|---|---|---|
| Broker username (`basic`) | `<PREFIX>_USERNAME` | `username` |
| Broker password (`basic`) | `<PREFIX>_PASSWORD` | `password` |
| Broker token (`bearer`) | `<PREFIX>_TOKEN` | `token` |
| MCP dev token (`static` only) | `DEV_TOKEN` | `dev-token` |

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

## Observability

The server's observability switches are `OBS_*` env vars
([`docs/observability.md`](../../../docs/observability.md) § Feature Switches).
Each one has its own value here; there is no free-form flag map. The chart
renders an env var only when its value differs from the server's default, so
the defaults render none, and `observability.metrics.bindAddress` reaches the
config only when it is set.

| Value | Env var / config key | Default |
|---|---|---|
| `observability.otlpEndpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` (OTLP over gRPC, e.g. `http://otel-collector.observability.svc:4317`) | `""` (unset) |
| `observability.correlationId.enabled` | `OBS_CORRELATION_ID_ENABLED` | `true` |
| `observability.tracing.enabled` | `OBS_TRACING_ENABLED` | `false` |
| `observability.auditLog.enabled` | `OBS_AUDIT_LOG_ENABLED` | `false` |
| `observability.saturationEvents.enabled` | `OBS_SATURATION_EVENTS_ENABLED` | `false` |
| `observability.authFailureCounter.enabled` | `OBS_AUTH_FAILURE_COUNTER_ENABLED` | `null`: unset, so it follows the two metrics switches. `false` forces it off |
| `observability.metrics.scrape.enabled` | `OBS_METRICS_SCRAPE_ENABLED`: opens `/metrics` | `false` |
| `observability.metrics.otlp.enabled` | `OBS_METRICS_OTLP_ENABLED`: pushes metrics over OTLP | `false` |
| `observability.metrics.bindAddress` | `observability.metrics_bind_address` | `null`: the server default `":9091"` |
| `observability.serviceMonitor.enabled` | renders a ServiceMonitor and turns on `OBS_METRICS_SCRAPE_ENABLED` | `false` |
| `observability.serviceMonitor.additionalLabels` | extra ServiceMonitor labels | `{}` |

The chart refuses combinations that would do nothing without any error:

- **An endpoint nothing uses.** `otlpEndpoint` set with neither
  `tracing.enabled` nor `metrics.otlp.enabled` fails rendering.
- **An endpoint the exporter cannot read.** `otlpEndpoint` must be
  `http(s)://<host>[:port]`, with no credentials, path or query: a DNS name or
  IPv4 address, or an IPv6 address in brackets, and a port from 1 to 65535.
  Anything else is misread: a value that fails to parse, such as
  `10.0.0.5:4317`, leaves the exporter on `localhost:4317` with no error, and
  `host:4317` is read with `host` as its scheme, so the exporter dials it over
  TLS.
- **A metrics port nothing can reach.** The metrics containerPort, the
  Service's `metrics` port and the NetworkPolicy's metrics port all follow
  `metrics.bindAddress`. It must bind all interfaces (`":N"`, `"0.0.0.0:N"` or
  `"[::]:N"`), since the Service and Prometheus reach it through the pod IP,
  and its port must differ from `server.port`.
- **A ServiceMonitor with no listener.** `serviceMonitor.enabled` turns
  scraping on by itself.

Two combinations render, because a collector may be supplied some other way
(a sidecar, or an operator that injects `OTEL_EXPORTER_OTLP_ENDPOINT`):

- `metrics.otlp.enabled` with no endpoint: the server logs a startup warning
  and pushes to the OTel SDK default, `localhost:4317`.
- `tracing.enabled` with no endpoint: traces also go to `localhost:4317`, but
  **the server logs no warning for tracing**. The chart's install notes print
  one instead, unless `extraEnv` sets `OTEL_EXPORTER_OTLP_ENDPOINT` or
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`.

### Prometheus Operator

With `kube-prometheus-stack` (or any Prometheus Operator) already in the
cluster:

```yaml
observability:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack   # your Prometheus' Helm release name
```

The ServiceMonitor is created in the release namespace and selects only this
release's Service, on its `metrics` port. The `monitoring.coreos.com/v1` CRD
must be installed, or the install fails. Four things must hold before a target
appears, and only the first fails loudly:

1. Scraping is on. `serviceMonitor.enabled` turns on
   `OBS_METRICS_SCRAPE_ENABLED`; without it every scrape is `connection refused`.
2. The Service has a `metrics` port. The chart always renders it.
3. Prometheus selects this ServiceMonitor. kube-prometheus-stack by default
   selects only ServiceMonitors labelled `release: <its Helm release>`: set that
   label in `additionalLabels`, or set its
   `serviceMonitorSelectorNilUsesHelmValues` to `false`. If Prometheus runs in
   another namespace, its `serviceMonitorNamespaceSelector` must be `{}` or
   match this one. Neither mismatch is an error anywhere; the only symptom is no
   target.
4. Prometheus can reach the metrics port. With `networkPolicy.enabled`, set
   `networkPolicy.metricsNamespace` to the namespace Prometheus runs in.

`/metrics` is unauthenticated, and once scraping is on any pod that can route
to this one can read it. The chart's NetworkPolicy is off by default, unlike
`deploy/kubernetes/networkpolicy.yaml`, which a directory-wide apply includes:
turn it on alongside scraping. The install notes warn when scraping is on
without it.

Trace exemplars also need Prometheus-side settings this chart cannot supply
([`docs/observability.md`](../../../docs/observability.md) § Trace Exemplars).
Without the Operator, set `observability.metrics.scrape.enabled` and point your
own Prometheus scrape configuration at the Service's `metrics` port
(`docs/observability.md` § Scraping and Securing the Metrics Endpoint).

### OpenTelemetry collector

Point the server at an existing collector's OTLP/gRPC receiver, and turn on
what it should export:

```yaml
observability:
  otlpEndpoint: http://otel-collector.observability.svc:4317
  tracing:
    enabled: true
  metrics:
    otlp:
      enabled: true
```

The server pushes OTLP over gRPC only, so use the collector's `4317` port, not
`4318`. Other `OTEL_*` settings (`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`,
`OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`,
`OTEL_METRICS_EXEMPLAR_FILTER`) go in [`extraEnv`](#extra-environment-variables).
OTLP certificate files need a mounted file, which this chart cannot yet supply
(SOL-156417). If something else restricts this pod's egress, allow TCP 4317 to
the collector there: the chart's NetworkPolicy is ingress-only.

### Grafana

Import [`deploy/grafana/solace-broker-mcp-overview.json`](../../grafana/solace-broker-mcp-overview.json)
into your Grafana and bind its Prometheus data-source input. It works with
scraped metrics and with OTLP metrics ingested into Prometheus through a
collector ([`docs/observability.md`](../../../docs/observability.md) § Grafana
Dashboard).

## NetworkPolicy

`networkPolicy.enabled` renders an ingress-only policy, adapted from
[`deploy/kubernetes/networkpolicy.yaml`](../../kubernetes/networkpolicy.yaml):
the MCP port (`server.port`) is open to any source, since the kubelet's probes
come from the node's own address, and the metrics port only to
`networkPolicy.metricsNamespace`. Egress (brokers, IdP, DNS, OTLP) is
untouched. Two reasons an applied policy may protect nothing: only a CNI that
implements NetworkPolicy enforces it, and policies are additive, so another
policy selecting these pods can re-open the metrics port. Verify from a pod
outside that namespace that the metrics port does not answer.

## Ingress

`ingress.enabled` renders an Ingress equivalent to
[`deploy/kubernetes/ingress.yaml.example`](../../kubernetes/ingress.yaml.example),
routing `/mcp` and `/.well-known` (OAuth discovery) to the Service:

```yaml
ingress:
  enabled: true
  className: nginx
  host: mcp.example.com
  tls:
    secretName: mcp-example-tls
```

> **Under `mcpClientAuth.mode: static`, publishing the server exposes the
> shared `DEV_TOKEN`**, its only credential: anyone who can reach the host and
> holds the token gets broker-admin-backed access. Switch to
> [OAuth](#client-authentication) first, or keep the Ingress on a trusted
> network.

Set `ingress.tls.secretName`. Without it the Ingress serves plain HTTP, unless
TLS terminates in front of it, and every client's bearer token crosses that
network in cleartext; the install notes warn in that case.

**Session affinity is required above one replica.** MCP sessions live in one
pod's memory, and an Ingress bypasses the Service's `sessionAffinity`, so
without affinity at the Ingress, requests that land on another pod get
`404 session not found`. The default annotations provide it for ingress-nginx:

| Annotation | Value | Why |
|---|---|---|
| `nginx.ingress.kubernetes.io/upstream-hash-by` | `"$remote_addr"` | Pins each client to one pod. Required, not tuning ([`docs/authentication.md`](../../../docs/authentication.md) § Session Routing at the Ingress) |
| `nginx.ingress.kubernetes.io/proxy-read-timeout` | `"7200"` | The SSE stream on `GET /mcp` must outlive the 2h session idle timeout |
| `nginx.ingress.kubernetes.io/proxy-send-timeout` | `"7200"` | Same |
| `nginx.ingress.kubernetes.io/proxy-buffering` | `"off"` | Buffering defeats the SSE stream |

These annotations are ingress-nginx-specific. Other controllers ignore them
silently: the Ingress reconciles cleanly and then 404s on session routing. For
another controller, set each of these keys to `null` and pin each client by
its source address with that controller's consistent hashing
([`docs/authentication.md`](../../../docs/authentication.md) § Gateway API and
Service Meshes names the Istio and Envoy Gateway settings), or run
`replicaCount: 1`. Neither cookie affinity nor hashing on `Mcp-Session-Id`
works: both apply cleanly, then fail for clients that keep no cookie jar, the
Go SDK among them (same document, § What Does Not Work).

```yaml
ingress:
  className: traefik
  annotations:
    nginx.ingress.kubernetes.io/upstream-hash-by: null
    nginx.ingress.kubernetes.io/proxy-read-timeout: null
    nginx.ingress.kubernetes.io/proxy-send-timeout: null
    nginx.ingress.kubernetes.io/proxy-buffering: null
    # ...your controller's source-address hashing and timeout settings
```

## Extra environment variables

`extraEnv` appends full Kubernetes `EnvVar` entries to the container, so a
value can come from a Secret (`valueFrom.secretKeyRef`) as well as a literal
`value`. Rendering fails if an entry sets an `OBS_*` variable or
`MCP_SERVER_PORT` (use the named values instead), a name the chart already
sets (`CONFIG_FILE`, `POD_NAME`, `GOMEMLIMIT`, a credential variable, or
`OTEL_EXPORTER_OTLP_ENDPOINT` when `observability.otlpEndpoint` is set), or the
same name twice: Kubernetes would keep only the last one, silently.

### Egress proxy

The server routes outbound HTTP through the standard `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY` variables
([`docs/configuration.md`](../../../docs/configuration.md) § Outbound HTTP
Proxy). Supply them through `extraEnv`. A proxy URL with credentials
(`http://user:password@proxy.example.com:3128`) is a secret, so take it from a
Secret rather than a literal:

```bash
kubectl create secret generic egress-proxy \
  --from-literal=https-proxy-url='http://user:<password>@proxy.example.com:3128'
```

```yaml
extraEnv:
  - name: HTTPS_PROXY
    valueFrom:
      secretKeyRef:
        name: egress-proxy
        key: https-proxy-url
  - name: NO_PROXY
    value: ".svc,.svc.cluster.local,otel-collector.observability.svc"
```

**OTLP export always follows `HTTPS_PROXY`, whatever the endpoint's scheme.**
Export is gRPC, which resolves its proxy against a synthesized `https` target,
so an `http://...:4317` collector endpoint is not governed by `HTTP_PROXY`. To
reach an in-cluster collector directly, list its host in `NO_PROXY` exactly as
`observability.otlpEndpoint` spells it: `NO_PROXY` matches the host as written,
not after DNS resolution.

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
over the rendered chart, the helm-unittest suites in `tests/`, and the drift
check against [`deploy/kubernetes/`](../../kubernetes/) with its self-test.
`ci/optional-resources-values.yaml` turns every optional resource on. Its
ServiceMonitor is skipped by that `kubeconform` pass, which uses built-in
schemas only, and is validated against the CRD schema catalog in `ci-pr.yaml`'s
"Kubernetes manifests valid" job instead. Bump
`version` in `Chart.yaml` in every pull request that changes the chart.

Tools, at the versions CI pins:

| Tool | Version |
|---|---|
| [Helm](https://helm.sh/docs/intro/install/) | v3.22.x |
| [helm-unittest](https://github.com/helm-unittest/helm-unittest) plugin | v1.2.x (pinned with Helm v3.22: helm-unittest v1.1+ uses a plugin field Helm 3.17 rejects) |
| [chart-testing](https://github.com/helm/chart-testing) (`ct`) | v3.15.0, with its `etc/` files in `~/.ct` |
| `yamllint` and `yamale` (Python, used by `ct lint`) | 1.38.0 and 6.1.0 |
| [kubeconform](https://github.com/yannh/kubeconform) | v0.6.7 |
| [yq](https://github.com/mikefarah/yq) (mikefarah, not the Python `yq`) | v4.54.1 |
| [dyff](https://github.com/homeport/dyff) | v1.12.0 |
| perl (the drift check self-test) | any 5.x (macOS and most Linux distributions ship one) |

### The drift check

[`.github/scripts/helm-parity-check.sh`](../../../.github/scripts/helm-parity-check.sh)
renders the chart twice and compares each render with the raw manifests,
matching resources by kind and name in both directions:

- `ci/parity-values.yaml` reproduces `kubectl apply -f deploy/kubernetes/` and
  is compared with every file that apply reads there (`*.json`, `*.yaml`,
  `*.yml`).
- `ci/parity-examples-values.yaml` also turns on the ServiceMonitor and
  Ingress; only those are compared, with the `*.example` files. The check
  also fails unless this file is `ci/parity-values.yaml` plus those two
  resources' values.

Before comparing, both sides go through a short, fixed allow-list of intended
differences, each commented in the script: the labels only Helm sets,
`app.kubernetes.io/instance` (the chart also selects on it, so two releases can
share a namespace), and the pod template's checksum annotations. Those keys
belong to the chart alone: a raw file carrying one fails the check, since the
normalisation would otherwise hide that edit. The
ConfigMap's `config.yaml` is compared as parsed YAML, and the Secret by key
names only, under `stringData` and `data` alike, so no Secret value appears in
a report. Comments are not compared, so the raw files' commented-out
alternatives are covered by helm-unittest cases instead. Any other difference
fails the job with a `dyff` report naming the field.

The comparison reads YAML 1.2, but kubectl and Helm read YAML 1.1, where an
unquoted `off`, `on`, `yes`, `no`, `y` or `n` is a boolean. So an unquoted one
on either side fails the check too, even when both sides agree: quote it. So
does a document on either side with no `kind` or `metadata.name`, which the
kind-and-name matching would otherwise skip, and a Secret value that is not a
string (an unquoted number), which kubectl rejects.

When it fails, change the other side to match. A new raw manifest or `.example`
file needs a chart counterpart and, usually, a line in one of the two parity
values files. Adding an entry to the allow-list is a reviewed change: it must
describe an intended difference, not paper over drift.
