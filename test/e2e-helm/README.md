# E2E Helm Chart Suite

End-to-end test for the Helm chart at
[`deploy/charts/solace-broker-mcp/`](../../deploy/charts/solace-broker-mcp/README.md)
(SOL-156328): installs the chart from this checkout into an ephemeral `kind` cluster,
against two real Solace brokers, and drives live MCP tool calls through it. It also
guards the env-var collision bug with a decoy Service and a negative control.

Runs in [`helm-chart-e2e.yml`](../../.github/workflows/helm-chart-e2e.yml) after each
successful nightly dev build, and on manual dispatch; not on pull requests. Design and rationale:
[`docs/_internal/e2e-testing.md`](../../docs/_internal/e2e-testing.md#helm-chart-suite--teste2e-helm).

Builds on the shared scaffold in [`../e2e-common/`](../e2e-common/README.md).

## Quickstart

```bash
make e2e-helm-all     # cluster up, brokers up, install, tests, teardown (even on failure)
make e2e-helm-down    # clean up after an interrupted run
```

Unlike the other suites, `run-all.sh` owns the whole lifecycle, so `make e2e-helm-all`
is just `bash test/e2e-helm/run-all.sh`. It is the same script the workflow runs.

## Prerequisites

- An **x86_64** host. The dev image is built for `linux/amd64` only (`dev-build.yaml`),
  so `kind` on arm64 (e.g. Apple Silicon) is not expected to run it. Not tested on arm64.
- Docker with Compose, `kind` (the suite pins the version in `.env` and warns on a
  mismatch), `kubectl`, `helm`, `curl`, `jq`.
- Pull access to `ghcr.io/solaceproducts/solace-broker-mcp-dev:dev` (public).

## Ports and names (distinct from the other suites, see `.env`)

| | broker-a | broker-b |
|---|---|---|
| Container | `solace-e2e-helm-a` | `solace-e2e-helm-b` |
| SEMP port (host) | `8106` | `8108` |
| Chart credentials | chart-managed, user `e2ehelma` | `existingSecret` (`broker-b-credentials`), user `e2ehelmb` |

MCP server: the chart's Service, port-forwarded to `127.0.0.1:9097`. kind cluster:
`sol-e2e-helm`, with its own kubeconfig at `bin/kubeconfig` (your `~/.kube/config`
is not touched); namespace `mcp-e2e`; release `e2e`.

Each broker has the shared `admin` user, which only the suite's own setup uses, plus
a chart user that exists on that broker alone. Credentials the chart routed to the
wrong broker therefore fail authentication rather than passing on a shared login.

One run per host: concurrent runs share the cluster name, containers and ports, so
`run-all.sh` and `make e2e-helm-down` take a host-wide lock (`/tmp/sol-e2e-helm.lock`,
via `flock`). `run-all.sh` refuses to start while another run holds it; `make
e2e-helm-down` waits up to 120s first, so it does not race a killed run's teardown
that is still finishing.
