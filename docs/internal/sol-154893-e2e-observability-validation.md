# E2E observability validation — SOL-154893

Date: 2026-09-29

Link: https://sol-jira.atlassian.net/browse/SOL-154893
Parent epic: SOL-153075 (Broker MCP GA Readiness)

Story: stand up the published Broker MCP binary with all observability flags on, connect
it to a real lab broker, Prometheus, and Grafana, run tool traffic through it, and document
issues or pain points. Acceptance goal: push and pull observability both work, verified by
a tool request visible on a Grafana chart. This is a validation exercise with no code
deliverable of its own.

## Environment

- Devserver: `mfenelon@dev2-26` (192.168.2.26), stack at
  `/opt/sbox/mfenelon/sol-154893-observability/` (moved off the home directory because NFS
  root-squash there breaks Docker bind mounts).
- Image under test: `ghcr.io/solaceproducts/solace-broker-mcp:latest`, resolving to the
  published `v0.9.0` release — not a local or dev build.
- Broker: `lab-129-78` (default lab credentials, reachable over the default VPN).
- Stack: `mcp-server` + `prometheus-scrape` (pull path) + `otel-collector` + `prometheus-otlp`
  (push path) + `tempo` (traces) + `grafana`, modeled on
  `test/e2e-dashboard/docker-compose.yml` (metrics and Grafana) and
  `test/e2e-tracing/docker-compose.yml` (Tempo).
- All `OBS_*` flags on. Grafana exposed on the LAN (`192.168.2.26:3000`, default Grafana credentials)
  after a plain `ssh -L` port-forward from a Mac to the devserver failed (`channel N: open
  failed: connect failed: open failed`, likely an sshd `PermitOpen` restriction that could
  not be confirmed without root on the devserver).
- Live traffic: a background script (`bin/live-traffic.sh`) issued MCP tool calls every 5s
  so Grafana panels updated with real data during validation. Stopped once validation
  finished.

## Result

Both metrics paths (Prometheus scrape and OTLP push), both Grafana dashboards, and tracing
through Tempo were independently verified working with real data from `lab-129-78`. The
acceptance goal — a tool request visible on a Grafana chart, for both the push and pull
paths — is met.

## Findings

### 1. Docs describe a flag name the currently published release does not have

`docs/observability.md` on `main` documents `OBS_METRICS_SCRAPE_ENABLED` as the flag that
enables Prometheus scrape metrics, and describes the older `OBS_METRICS_ENABLED` name as
retired and ignored. That rename shipped in commit `db29c30` (SOL-154607), which is **not**
in the `v0.9.0` tag — `git merge-base --is-ancestor v0.9.0 db29c30` confirms `v0.9.0` is an
ancestor of `db29c30`, and `git rev-list --count v0.9.0..db29c30` returns 25 commits ahead.
`v0.9.0` is the newest tag and the release published as
`ghcr.io/solaceproducts/solace-broker-mcp:latest` today.

Following the current docs against the actual released image fails: the `v0.9.0` binary
still requires the pre-rename flag, and setting only `OBS_METRICS_SCRAPE_ENABLED` (as
documented) leaves that requirement unmet. The observed startup failure is:

```text
OBS_METRICS_OTLP_ENABLED=true requires OBS_METRICS_ENABLED=true
```

A new user following `docs/observability.md` verbatim against the released binary hits this
hard failure and has no path to the fix from the docs. This is the one finding that is a
real, actionable doc gap — addressed directly in this PR with a callout in
`docs/observability.md` noting that pre-`db29c30` releases need the legacy flag name.

### 2. `list-queues` / `list-rdps` showed 100% errors in the live-traffic panels — not a bug

Investigated because the error-outcome panels were unexpectedly busy. `list-queues` and
`list-rdps` require a `msgVpnName` argument that the validation traffic script did not pass;
tools like `list-brokers`, `get-broker-status`, and `list-vpns` don't need one and succeeded
normally. This is correct validation behavior — the traffic script's own gap, not a server
defect — and it usefully demonstrated that the error-outcome panels populate correctly with
real data.

### 3. NFS root-squash breaks Docker bind mounts from the home directory (devserver-specific)

`dev2-26`'s home directory is NFS-mounted with root-squash, which breaks Docker bind mounts
sourced from `~`. Worked around by building the stack under `/opt/sbox/mfenelon/` (local
disk) instead. This is a devserver environment property, not a Broker MCP issue; noted here
for anyone repeating this validation on the same class of host.

### 4. Plain SSH port-forward to the devserver's Grafana did not work (unresolved, non-blocking)

`ssh -L` from a Mac to `dev2-26`'s Grafana port failed with `channel N: open failed: connect
failed: open failed`. This looks like an sshd `PermitOpen` restriction on the devserver, but
could not be confirmed without root there. Worked around by exposing Grafana on the LAN
(`192.168.2.26:3000`) instead, with explicit sign-off for that exposure. Doesn't block this
ticket; flagging in case someone with devserver admin access wants to fix port-forwarding
for future validation work.

## Out of scope

This ticket made no functional code changes beyond the doc callout for finding #1. It did
not touch the MCP server, the composite tool definitions, or the E2E test suites.
