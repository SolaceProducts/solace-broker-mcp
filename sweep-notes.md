# andrea-sweep session notes — solace-broker-mcp

Started: 2026-08-31
Invocation: /andrea-developer wrapping /andrea-sweep, N=3, performance added as explicit scan axis,
worktrees + parallel subagents authorized for the fix phase to avoid cross-ticket conflicts.

## Phase 1 — Configuration (baked-in + mid-turn user correction)

| Setting | Value |
|---|---|
| Issue count (N) | 3 |
| Jira project | SOL |
| Jira cloud | sol-jira.atlassian.net |
| Issue type | Task (id 10101) — verify on first create |
| Component | MCPBroker (components id 17857) — verify on first create |
| Product | MCPBroker (customfield_10200 option id 19593) — verify on first create |
| Assignee | andrea.ross@solace.com — resolve via lookupJiraAccountId |
| Parent epic | **SOL-153075** (user correction mid-turn, overrides "none" default) |
| Priority | High or Medium per triage |
| Slack channel | #dax (announce each PR) |

Jira MCP: mcp__plugin_docs-review_atlassian__* — createJiraIssue present. Confirmed.
Slack MCP: mcp__claude_ai_Slack__slack_send_message present. Confirmed.
/good:fix available. Confirmed.

## Repo orientation (understand-explain)

`.ua/` knowledge graph exists but is STALE vs HEAD (graph commit 72e822c, HEAD c949e9a).
Diff includes: internal/idpclient/retrying.go, internal/tokenexchange/retry_after_gate.go,
internal/oauth/cache/cache.go, cmd/server/body_limit_test.go, cmd/server/session_timeout_test.go,
internal/tokenexchange/exchange.go/errors.go, internal/semp/auth/oauth.go, internal/tools/manager.go,
internal/tools/validation.go, test/performance/* (load/perf harness), etc. — i.e. a decent chunk of
auth/retry/token-exchange code changed since the graph was built. Treating the graph as orientation
only; reading affected code directly rather than trusting graph summaries for anything touched above.

Layer map (from graph, still structurally valid):
- layer:entry-point — cmd/server bootstrap (14 nodes)
- layer:mcp-tools — tool registration/dispatch incl. composite YAML tools (117 nodes)
- layer:semp-client — HTTP client to brokers over SEMP v1/v2, pooling, auth (50 nodes)
- layer:auth-identity — request auth/authz, OAuth token cache, IDP client, token exchange (54 nodes)
- layer:observability — audit logging, sanitization, correlation IDs, health/metrics (20 nodes)
- layer:runtime-support — YAML config, defaults, startup warnings (19 nodes)

## Pre-existing worktrees (NOT part of this sweep — avoid duplicate findings)

- .claude/worktrees/sol-152979-broker-url-log-leak (branch aross/SOL-152979-broker-url-log-leak)
- .claude/worktrees/sol-152981-tokenexchange-audience-trust (branch aross/SOL-152981-tokenexchange-audience-trust,
  already merged to main per git log — PR #281)
Also merged already: aross/SOL-152980-calltool-error-visibility (branch exists, check if merged).
Scan subagents are told to skip findings duplicating: broker URL leaking into logs, token-exchange
audience trust/dedup, calltool error visibility — those are handled elsewhere.

## Phase 2 — Scan axes (dispatched in parallel, read-only, tracked files only)

1. Input validation / deserialization / parsing boundaries
2. Concurrency: races, deadlocks, goroutine lifecycles
3. Error handling, crash recovery, resource leaks
4. Auth/authz, secret/credential handling
5. Network/external boundaries: timeouts, retries, partial failures
6. Logging: credential/PII leaks (folds in /check-logs rules)
7. Performance: hot-path allocations, N+1 SEMP calls, unbounded pagination/growth, lock contention,
   algorithmic complexity under load (explicit axis added per user request)

(Findings appended below as subagents return.)

## Phase 3 — Triage shortlist (user-approved)

Survived triage (3, matches N, no padding — one performance-flavored logging finding
dropped as a stylistic nit with low real exposure per the scanning subagent's own admission):

1. Performance: `internal/composite/executor.go` `ResolveArgs` re-parses Go templates on
   every step call instead of compiling once at load (`internal/composite/definition.go`,
   `loader.go`).
2. Robustness/observability: malformed or omitted MCP `tools/call` arguments bypass the
   audit log entirely — `internal/tools/register.go:165-178`.
3. Security/logging: `SEMPError.Error()` falls back to a raw, unaudited response body when
   `meta.error` parsing fails, logged verbatim in `internal/tools/manager.go` despite a
   comment claiming the type carries no credentials — `internal/semp/sempv2/client.go:64-69`.

## Phase 4 — Filed tickets (parent epic SOL-153075, project SOL, component/product MCPBroker,
assignee Andrea Ross, priority Medium — all confirmed by read-back on the first ticket)

- SOL-153764 — https://sol-jira.atlassian.net/browse/SOL-153764 — composite template re-parse (perf)
- SOL-153765 — https://sol-jira.atlassian.net/browse/SOL-153765 — audit-log gap on malformed args
- SOL-153766 — https://sol-jira.atlassian.net/browse/SOL-153766 — SEMPError raw-body logging

## Phase 5 — Fixes (parallel via worktrees + subagents, per user request)

Worktrees created off origin/main:
- .claude/worktrees/sol-153764-composite-template-cache — branch aross/SOL-153764-composite-template-cache
- .claude/worktrees/sol-153765-calltool-args-audit-gap — branch aross/SOL-153765-calltool-args-audit-gap
- .claude/worktrees/sol-153766-sempv2-error-body-logging — branch aross/SOL-153766-sempv2-error-body-logging

No file overlap between the three tickets' scopes, so worktrees are purely to allow parallel
branches/subagents, not to avoid a shared-file merge conflict.

Plan phase (investigate + plan + Fable plan review) dispatched to 3 parallel subagents,
each to STOP after producing a Fable-reviewed plan for batched user approval before any
code is written. User approved all 3 plans in one batch; implementation dispatched to the
same 3 subagents in parallel.

## CLOSED OUT — all 3 shipped

| Ticket | PR | Notes |
|---|---|---|
| SOL-153764 (perf: template caching) | https://github.com/SolaceProducts/solace-broker-mcp/pull/352 | Fable diff review did mutation testing (reverted fix, confirmed new tests fail) |
| SOL-153765 (audit-log gap) | https://github.com/SolaceProducts/solace-broker-mcp/pull/353 | Fable caught a `buildLocalErrorResult` contract violation in the first draft |
| SOL-153766 (SEMPError body logging) | https://github.com/SolaceProducts/solace-broker-mcp/pull/351 | Fable caught a wrap-chain-losing helper design in the first draft |

All: make check clean before/after, red-green TDD + revert-verify, Fable plan review +
Fable diff review + self /good:review, CHANGELOG [Unreleased] entry, Jira comment, #dax
announcement. Worktrees left in place (branches pushed, nothing uncommitted) pending PR
review/merge — same pattern as the pre-existing sol-152979/sol-152981 worktrees.

Nothing deferred to a future sweep. Next: set story points on all 3 tickets (not done by
this skill), review the 3 PRs, remove worktrees after merge.
