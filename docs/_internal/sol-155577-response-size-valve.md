# SOL-155577 — Response-size safety valve for composite tools

Date: 2026-10-22

Link: https://sol-jira.atlassian.net/browse/SOL-155577
Origin: SOL-155426 / PR #484 (review note from @bczoma, non-blocking there)

Status: implemented in the PR that adds this file.

## Context

SOL-155426 fixed one measured overflow: `list-queue-discards` at its documented
default returned 72,416 characters, which a real MCP host refused. The fix was a
per-tool opt-in, `omitRawSteps`. Nothing generic stops the next tool from doing
the same, and nothing tells an operator or the agent when a result has grown
large. The package doc in `internal/composite/postprocess/postprocess.go` states
a convention (a handler's summary must bound itself), but nothing checked it, and
it does not cover `collect` tools, which return the same kind of raw array.

This record answers the five open questions in the ticket, with measurements.

## What the server actually sends

Reading `ToolManager.buildValidatedResult` (`internal/tools/manager.go`) changed
the shape of the problem:

- The result is marshaled **once**, to validate it against the output schema.
  Those compact bytes are then re-indented into a `TextContent` fallback. Every
  tool result therefore travels **twice**: as `structuredContent` and as indented
  text. For a real 100-queue `list-queues` call the compact payload was 34,936
  bytes and the text 49,468.
- Item count is already capped. `fetchPaginated` stops at `maxResults` (default
  100, ceiling 500) and sets `truncated` / `truncatedMessage` on the step. So an
  item-count ceiling would add nothing: the 500-row ceiling already exists, and
  what varies is the **width** of a row (about 10 times between tools: roughly 70 bytes per subscription row against 700 per VPN or discard row).

## Measurements

Method. Every shipped read-only composite tool (23 of them) was run through the
real executor, with a SEMP stand-in that answers each request from the step's own
`select` list and pages with `count` / `cursor` as the broker does. Cases: the
documented default (100) and the `maxResults` ceiling (500, for tools that declare
it), each with typical and worst-case field widths (longest names and failure
reasons the broker accepts). Categorical fields (profile, platform, state,
failure reason) draw from a vocabulary of 8 values, as a real fleet's do; object
names are unique per row. The harness is now a permanent test
(`internal/composite/response_cap_shipped_test.go`).

Calibration against a real broker (Docker `solace-pubsub-standard`): a real
`list-queues` row with a 70-character queue name is 346 bytes; the harness
produces about 356 bytes per row at typical widths. A queue with 500 real
subscriptions (200-character topics) returned 180,335 bytes from
`list-queue-subscriptions` before the valve.

Compact-JSON size of the whole result, bytes, before the valve:

| Tool | Default 100, typical | Maximum 500, worst case |
|---|---:|---:|
| `list-vpns` | 66,984 | 652,808 |
| `get-rdp-status` (100 bindings + 100 consumers) | 83,389 | 188,771 (no `maxResults`; worst-case widths) |
| `list-kafka-receivers` | 39,432 | 422,876 |
| `list-queues` | 35,628 | 240,872 |
| `list-slow-subscribers` | 34,832 | 308,503 |
| `list-clients` | 27,929 | 265,773 |
| `list-queue-subscriptions` | 6,752 | 137,848 |
| `list-queue-discards` (after #484) | 1,733 | 2,677 |
| single-resource `get-*` tools | 287 – 1,028 | up to 2,466 (worst-case widths) |

Reading the table:

1. At the documented default, most tools are 7–40 KB. Two (`get-rdp-status`,
   `list-vpns`) exceed 64 KiB only when the broker really holds 100 bindings plus
   100 consumers, or 100 VPNs.
2. At the ceiling, **every list tool** is far over: 138 KB to 653 KB. The ceiling
   is a documented input, so this is a supported call, not a misuse.
3. The incident response (72,416 characters) sits between the default sizes
   above and the ceiling sizes. A cap near the default sizes would have caught it.

## The five questions

### 1. Unit: bytes of compact JSON

Decision: **bytes of the compact JSON `structuredContent`**, cap
`composite.ResponseCapBytes = 64 KiB` (65,536).

- Tokens are the host's concept and differ by host and tokenizer; the server
  cannot count them. Bytes are a monotone proxy for them. For orientation only:
  one widely used MCP host is reported to refuse a tool result above 25,000 tokens
  by default (third-party sources; not verified against the vendor's own
  documentation), and at about 3 to 4 bytes per token of JSON the incident's
  72 KB compact (about 100 KB of indented text) is the right order of magnitude to
  have failed.
- Item count was rejected: it is already capped at 500, and the measured spread
  in row width (about 10x between tools) means a count says nothing about size.
- The cap is below the one size known to have failed (72,416 characters) and
  above every shipped tool at its documented default with realistic data, except
  the two named above.
- Bytes of the compact form, not of the indented text: the compact form is what
  validation already produces, and the indent factor is a near-constant 1.15–1.4.

### 2. Where to measure: reuse the marshal that already happens

Decision: **measure in `buildValidatedResult`, from the byte length validation
already computed.** Cost on the normal path: one integer comparison.

- Marshaling in `ApplyResultStrategy` just to check would double the marshal on
  every call. The executor also cannot see the whole payload: the strict
  output-schema step keys and the `isError` flag live in the manager.
- A cheaper proxy (item count before serialization) fails for the reason above.
- The over-cap path does extra work (one marshal per row, one re-validation), but
  only for a result already too large to send, which is rare.
- The logic lives in the composite package (`composite.BoundResponse`); the manager
  reaches it through an optional interface, `tools.ResponseBounder`, that
  `CompositeToolHandler` implements. Native tools do not implement it and are
  never trimmed (all of them return small, fixed-shape status objects).

### 3. Scope: composite tools, both strategies

Decision: **every composite tool, `collect` and `postProcess` alike,** because
the over-cap data is in step `data` arrays in both strategies (the incident's
72 KB was raw rows; `list-clients` at the ceiling is 266 KB of the same shape).
Write tools implement the interface too, but never reach the cap (a result is one
object); the manager skips the valve for error results.

### 4. Enforcement: truncate and say so, never fail

Decision: **remove tail rows, flag the step, count and log it.** Not a hard error,
not a log-only warning.

- A hard error turns a result that is mostly fine into nothing, and the agent
  cannot always act on it. Truncation keeps the useful head of the data.
- A log-only warning leaves the host overflow in place, which is the incident.
- How: rows come off the end of the **largest** `data` array first, one at a time,
  so arrays shrink toward equal size; ties break on step ID. The same input always
  gives the same output. Step maps are copied, not edited in place.
- What the agent sees, on each step that lost rows, reusing the keys paginated
  truncation already uses: `truncated: true`, `omittedItems: N`, and a
  `truncatedMessage` that states kept/total, the cap, whether the summary is still
  complete, and how to narrow the query. An earlier `maxResults` message is kept
  and the size note is appended. The text contains counts and the cap only, never
  a parameter value or a row.
- A `postProcess` summary is computed before trimming and is never touched, so its
  counts still describe every row fetched.
- If trimming every row still would not fit (the excess is in a summary), nothing
  is changed: degrading the result for no gain is worse. It is reported as
  `unbounded`, see Failure modes.
- A trimmed result is validated against the output schema again. A tool whose schema
  forbids the added keys fails loudly rather than sending an invalid result.
- Observability: counter `mcp_response_bounded_total{tool,broker,outcome}` with
  `outcome` in `truncated`, `unbounded` (metrics schema 1.10 to 1.11), and one WARN
  log line with counts only. No new error type: the call still succeeds, so the
  closed `error_type` set and the audit drift tests are untouched.

### 5. Boot-time validation: not a lint; a measured CI test

Decision: **no boot-time lint. A runtime valve plus a CI test that measures.**

- A boot check could only reject a definition whose shape "has no cap". Every
  paginated step is already capped at 500 items. What is unbounded is the **width**
  of a row and the cardinality of a summary, and neither is declared anywhere in
  `tools.yaml`: a `select:` list names fields, not their lengths. A declared byte
  budget per handler would be a number nobody can verify against reality, and it
  would drift.
- A check that *measures* does not drift. `TestShippedReadTools_StayUnderTheResponseCap`
  runs every shipped read tool at default and maximum input in typical and worst
  widths, and fails if any result is still over the cap after the valve, or if the
  valve reports `unbounded`. `TestShippedPostProcessSummaries_AreSmallAgainstTheCap`
  fails if any summary exceeds a quarter of the cap. A new tool is covered the
  moment it is in `tools.yaml`.
- Both layers are needed: the runtime valve covers what the test cannot see
  (real data wider than the model), and the test covers what the valve cannot
  trim (a summary). Neither alone is enough.

Mutation checks, run and recorded: removing the `topOffenderLimit` cap in
`list_queue_discards.go` fails both drift tests; every behavior of `BoundResponse`
and its manager wiring (inclusive boundary, metadata reserve, largest-first,
`omittedItems`, feasibility, message append, error-result skip, metric, re-validation,
outcome label) fails at least one test when removed.

## Alternatives considered

| Alternative | Why it lost |
|---|---|
| Item-count ceiling | Already exists (500); does not capture row width |
| Token estimate | Host-specific; the server cannot know it; adds a heuristic that looks precise and is not |
| Hard error over the cap | Loses data the agent could have used; not actionable for tools with no paging |
| Log-only warning | Does not prevent the overflow |
| Check in `ApplyResultStrategy` | Needs its own marshal on every call; cannot see the output schema or `isError` |
| Per-tool `maxBytes` in YAML | A number per tool, unverifiable, drifts; repeats the bespoke-flag problem |
| Configurable cap (config key, Helm value) | See below |
| Trim summary values too | A summary is semantic (counts, groupings); dropping keys silently makes figures wrong |
| Boot-time lint | See question 5 |
| Fix the two value-keyed summaries now | See "Found, not changed" |

### A constant, not configuration

The cap is a backstop for a tool that forgot to bound itself, not a tuning knob. A
config key would add a surface to `docs/configuration.md`, the Helm chart, and the
config-drift tests, for a value an operator can already steer by lowering
`maxResults`. If a deployment shows a real need for a different cap, the constant
is one line to make configurable and the manager field (`responseCap`) is the
single place it would be read. Documented as fixed in `docs/tools-reference.md`.

## Effect on existing tools

| Case | Effect |
|---|---|
| Result at or under 64 KiB (every tool at default except the two below, with realistic data) | None: byte-identical output, no new field |
| `list-vpns` with ~100 VPNs, `get-rdp-status` with ~100 bindings and ~100 consumers | A few rows omitted (4 and 34 in the synthetic run), step flagged, summary intact |
| Any list tool at `maxResults` 500 with wide rows | Rows trimmed to fit; step flagged with `omittedItems` |
| `list-queue-discards` | None (summary only, 1.7–2.7 KB) |
| Output shape | Unchanged for every result under the cap. Over the cap, existing keys keep their meaning; `omittedItems` is new and `truncated` / `truncatedMessage` are existing keys |

This is not marked BREAKING: no field is renamed or removed, and a result is only
changed when it is larger than a size that already failed a host in production.
A caller that relied on receiving all 500 wide rows of one call will now receive
fewer, and is told so. That is called out in the changelog and the PR so it can be
challenged.

## Failure modes

- **Summary too large** (`unbounded`). The valve cannot trim a summary, so a handler
  whose summary grows with the data is sent as built, counted as `outcome=unbounded`
  and logged. The CI tests are the first defence; the counter is the second.
- **A caller wants the omitted rows.** Most list tools have no cursor, so they are
  not reachable through that tool call: the message says to narrow the query. This
  is an existing limit of the tools (the 500 ceiling), now stated rather than silent.
- **Cap too low or high for a given host.** Cap is conservative against the one known
  failure; a host with a much smaller budget still needs a smaller `maxResults`.
- **Extra work on the over-cap path.** One marshal per row and a re-validation; paid
  only by an already-oversized result.
- **Schema rejects the added keys.** Fails as an output-validation error, visibly.
  Not reachable for shipped tools (monitor tools use the permissive envelope).

## Found, not changed

Measuring turned up a gap in the convention itself. Six postProcess handlers return
maps **keyed by a data value** (`byFailureReason`, `byInboundFailureReason`,
`byLastFailureReason`, `byBindingFailureReason`, `byClientProfile`, `byPlatform`):
their size is bounded by the number of *distinct* values, not by item count. With a
small vocabulary (how brokers behave) summaries stayed under 2.7 KB at the ceiling.
If every row carries a unique reason string, `list-bridges` reaches 102,593 bytes of
summary at 500 bridges, which is `unbounded`. That needs 500 bridges with distinct
200-character reasons; brokers hold far fewer bridges, so this is theoretical today.

It is not fixed here: it would change the output of six tools for a case that does
not occur, and the right bound (top-N by count plus an "other" count) is a product
choice. It is recorded here and in the PR as a follow-up.

## Not changed

- Native (SEMPv1) tools: small fixed-shape results, no list rows.
- `describe-semp-schema`: sized by design, already steers callers to a trimmed view.
- Tool descriptions: unchanged. Adding a sentence to ~20 descriptions would cost
  tool-list tokens on every `tools/list` for a rare case; the result carries its own
  explanation. The cap is documented in `docs/tools-reference.md`.
- No paging cursor for list tools.
