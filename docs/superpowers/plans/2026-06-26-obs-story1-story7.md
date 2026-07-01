# Plan: Observability Foundation — Story 1 + Story 7 (2026-06-26)

Part of the **Broker MCP Observability & Telemetry** epic ([SOL-150251](https://sol-jira.atlassian.net/browse/SOL-150251)), MUST child epic [SOL-149791](https://sol-jira.atlassian.net/browse/SOL-149791) "Pilot-Ready Foundation".

## Goal

Implement the first two MUST-tier stories as test-backed, independently reviewed branches:

- **[SOL-151278](https://sol-jira.atlassian.net/browse/SOL-151278) (Story 1)** — observability package skeleton + config/flag plumbing.
- **[SOL-151283](https://sol-jira.atlassian.net/browse/SOL-151283) (Story 7)** — `/livez` endpoint (process-alive, canonical); `/health` retained for backward compatibility with its original `{"status":"healthy"}` body.

These are the two stories with no upstream dependency on un-merged work, so they can be built without a reviewer present. Story 7 builds on the `internal/observability/health/` package that Story 1 creates, so it stacks on the Story 1 branch.

## Approach

- **Git worktrees, sequential.** Story 1 off `main`; Story 7 off the Story 1 branch.
- **TDD, matching existing idioms:** stdlib `testing`, `t.Setenv`, `httptest`, no testify; `${VAR}` YAML substitution; constants in `internal/defaults`.
- **Review gate:** each story is reviewed by an Opus agent acting as a skeptical principal architect (`/good:review`) before it is considered done. `make check` (build-all + vet + lint + test-race) must pass.

## Story 1 — scope

New packages, each with an `Enabled() bool` stub gated on its flag:
`internal/observability/{correlation,recover,metrics,audit,tracing,health}/`, plus `internal/observability/schema/version.go` (`MetricsSchemaVersion`, `AuditSchemaVersion = "1.0"`).

- `internal/auth/principal.go` — an unexported `principalKey{}` context-key type (matching the `rawSubjectTokenKey` idiom), empty `Principal` struct, and `PrincipalFrom(ctx) Principal` (population deferred to Story 20).
- `internal/config/observability.go` — `ObservabilityConfig`: env-driven `OBS_*_ENABLED` flags with door-closing v1 defaults (correlation + panic-recovery on; metrics, audit, tracing, saturation off; auth-failure-counter follows metrics; **no** `OBS_READYZ_STRICT_ENABLED`), and numeric thresholds (`saturation_threshold_ms`=10, `progress_signal_threshold_ms`=5000, `otel_self_stats_interval_s`=60) as `${VAR}`-substitutable YAML fields.
- Wire into `internal/config/config.go`, add constants to `internal/defaults/defaults.go`, and emit one INFO startup line in `cmd/server/main.go` summarizing the enabled capabilities (`"observability config loaded"` with a bool attr per flag).
- Unit tests: flag defaults, env overrides both directions, principal zero-value.

Story 1 wires **no behavior** into the request path. Skeleton + flags only.

## Story 7 — scope

- `/livez` handler returning `{"status":"alive"}` (process alive; flag-independent), housed in the health package.
- `/health` is retained as a backward-compatible path that preserves its original `{"status":"healthy"}` body (NOT a body-identical alias — changing it would break external consumers parsing `.status`); `/livez` is the canonical liveness endpoint; non-GET → 405. (Revised per reviewer feedback on PR #116.)
- Update the existing `TestHealth*` tests; verify the `--health` CLI flag, the Dockerfile `HEALTHCHECK`, and the K8s liveness probe depend only on the 200 status, not the body string.

## Risks / open

- **Story 7 adds `/livez` (`{"status":"alive"}`) and leaves `/health`'s shipped `{"status":"healthy"}` body unchanged** for backward compatibility. The canonical liveness path going forward is `/livez`.
- **Observability flags are env-driven**, slightly different from the YAML-first config; fit into `LoadConfig` cleanly.
- **Stacking:** if Story 1 changes in review, Story 7 rebases.

## Out of scope

- **Story 31** (`terminationGracePeriodSeconds` + SIGTERM drain) — deferred: its acceptance criteria depend on Story 8 (`/readyz`) and Story 31a (`SetShuttingDown`), neither in this batch.
- All other MUST/SHOULD/WOULD-BE-NICE stories.
- Any behavior beyond skeleton + flags; any broker code.
