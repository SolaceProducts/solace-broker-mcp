# Solace Broker MCP Server

Go MCP server exposing Solace broker monitoring/management tools over SEMP.
Architecture and diagrams: `docs/internal/architecture.md`.

## Commands

- `make check` — build, vet, lint, race-enabled tests. Matches CI's build/lint/test
  jobs (CI additionally runs the E2E jobs below); run before pushing.
- `make test` — unit tests. Single test: `go test ./internal/<pkg>/ -run TestName -v`
- `make e2e-all` — the `test/e2e-basic-mcp` suite against Dockerized brokers, full
  lifecycle (up, run, teardown). CI also runs the oauth and monitoring E2E suites
  (`docs/internal/e2e-testing.md`).
- CI (`.github/workflows/build-and-test.yml`) is the source of truth; if the Makefile drifts, CI wins.

## Adding a tool

Two mechanisms — prefer the first:

1. **Composite (YAML):** declare in `internal/composite/definitions/tools.yaml`
   (embedded at build time). Multi-step SEMPv2 calls, templates, pagination.
2. **Native Go handler:** `internal/tools/sempv1/`, one package per tool.
   Only for logic YAML can't express (e.g. SEMPv1 XML parsing).

Never declare a `broker` parameter — it is auto-injected at registration into
the schema of every tool defined through either mechanism (`injectBrokerParam`
in `internal/tools/register.go`). `list-brokers` and `describe-schema` are the
exceptions: registered separately, no `broker` parameter.

## Tool naming

MCP tool names use **kebab-case** — e.g. `get-broker-status`, `list-queues`,
`get-redundancy-status`. This applies to every tool the server exposes,
regardless of whether it's defined in YAML (composite) or implemented in
Go (native SEMPv1/SEMPv2). Avoid `snake_case` and `camelCase`.

Match the in-code constant or `Name:` field exactly to the on-the-wire
tool name; LLMs see and pattern-match against this string.

## Before committing

Run `/check-logs` to scan for logging security violations. Fix all CRITICAL
and HIGH issues before committing. Rules: `docs/internal/secure-logging-rules.md`.

Every commit needs a DCO `Signed-off-by` trailer — the `DCO sign-off` CI check
blocks a PR without one. `make hooks` (once per clone) installs a
`prepare-commit-msg` hook that adds it automatically; otherwise commit with
`git commit -s`.

## Before opening a PR

If the change touches production surface (`internal/config/`, tool definitions in
`internal/composite/definitions/tools.yaml`, or native tool packages under
`internal/tools/`), add a `CHANGELOG.md` entry under `[Unreleased]` — run
`/changelog` to draft one from your diff. CI posts an advisory warning (not a
block) on a behavior-changing PR with no `[Unreleased]` update, unless it carries
the `no-changelog` label. The hard gate is at release time: cutting a release
fails if the version's CHANGELOG block is missing, so entries must land before
then.
