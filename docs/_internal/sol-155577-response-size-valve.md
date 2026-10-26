# SOL-155577 — Response-size safety valve for composite tools

Date: 2026-10-22 (revised 2026-10-23 after review, see the last section)

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
  text, and the text is the larger copy (1.09x to 1.47x depending on row shape).
  For a real 100-queue `list-queues` call the compact payload was 34,936 bytes and
  the text 49,468; for a real 500-subscription `list-queue-subscriptions` call,
  64,655 and 74,380.
- Item count is already capped. `fetchPaginated` stops at `maxResults` (default
  100, ceiling 500) and sets `truncated` / `truncatedMessage` on the step. So an
  item-count ceiling would add nothing: the 500-row ceiling already exists, and
  what varies is the **width** of a row (about 10 times between tools: roughly 70
  bytes per subscription row against 700 per VPN or discard row).

## Measurements

Method. Every shipped read-only composite tool (23 of them) was run through the
real executor, with a SEMP stand-in that answers each request from the step's own
`select` list and pages with `count` / `cursor` as the broker does. Cases: the
documented default (100) and the `maxResults` ceiling (500, for tools that declare
it), each with typical and worst-case field widths (longest names and failure
reasons the broker accepts; in the worst case every enabled object is down with a
failure reason, so handlers that list failures are exercised). Categorical fields
(profile, platform, state, failure reason) draw from a vocabulary of 8 values, as a
real fleet's do; object names are unique per row. The harness is a permanent test
(`internal/composite/response_cap_shipped_test.go`).

Calibration against a real broker (Docker `solace-pubsub-standard`): a real
`list-queues` row with a 70-character queue name is 346 bytes compact; the harness
produces about 356 bytes per row at typical widths. A queue with 500 real
subscriptions (200-character topics) returned 180,335 bytes compact from
`list-queue-subscriptions` before the valve.

Size of the indented text of the whole result, bytes, before the valve:

| Tool | Default 100, typical | Maximum 500, worst case |
|---|---:|---:|
| `list-vpns` | 89,477 | 766,401 |
| `get-rdp-status` (100 bindings + 100 consumers) | 110,451 | 219,466 (no `maxResults`) |
| `list-kafka-receivers` | 53,970 | 497,232 |
| `list-queues` | 50,166 | 313,010 |
| `list-slow-subscribers` | 47,452 | 370,779 |
| `list-clients` | 38,430 | 317,874 |
| `list-queue-subscriptions` | 9,253 | 149,949 |
| `list-queue-discards` (after #484) | 2,316 | 3,260 |
| `get-vpn-status` | 1,323 | (single resource) |

After the valve, across all 72 cases the largest result is 65,534 bytes of text
(cap 65,536), none is reported `unbounded`, and the largest summary is 13,550
bytes.

Reading the table:

1. At the documented default, most tools are 2–54 KB of text. Two
   (`get-rdp-status`, `list-vpns`) exceed 64 KiB, and only when the broker really
   holds 100 bindings plus 100 consumers, or 100 VPNs.
2. At the ceiling, **every list tool** is far over: 150 KB to 766 KB. The ceiling
   is a documented input, so this is a supported call, not a misuse.

## The five questions

### 1. Unit: bytes of the indented JSON text

Decision: **bytes of the indented JSON text the model reads (`TextContent`)**, cap
`composite.ResponseCapBytes = 64 KiB` (65,536).

- Tokens are the host's concept and differ by host and tokenizer; the server
  cannot count them. Bytes are a monotone proxy for them. For orientation only:
  one widely used MCP host is reported to refuse a tool result above 25,000 tokens
  by default (third-party sources; not verified against the vendor's own
  documentation).
- Item count was rejected: it is already capped at 500, and the measured spread
  in row width (about 10x between tools) means a count says nothing about size.
- **Text, not the compact structured copy.** The first version measured the compact
  form and the review showed that trimmed results then reached the model as 70–91 KB
  of text. The text is the larger copy, so bounding it bounds both, and the manager
  already builds it for every call. A fixed correction factor (a cap divided by the
  worst indent ratio, or a lower cap) was rejected: the ratio ranges from 1.09 to
  1.47 with row shape, so any one factor over-trims the tight shapes by up to a
  third. The size is measured exactly instead.
- **The unit of the incident figure is not recorded.** The 72,416 characters in
  #484 does not say whether the compact or the indented copy was measured. If it
  was the text, 64 KiB of text is about 10% below it; if it was the compact form,
  the failing text was roughly 90–100 KB and 64 KiB of text is far below it. Gating
  on the text keeps the cap below the failure under either reading, which is why it
  is the unit. The margin is thin under the first reading, and no claim here depends
  on it being larger.
- The cap bounds the text, not the whole payload. A host that counts the structured
  copy as well sees about 1.7x to 2.5x the cap in total; the cap does not try to
  solve that.

### 2. Where to measure: reuse the text that is already built

Decision: **measure in `buildValidatedResult`, on the indented buffer it already
builds for `TextContent`.** Cost on the normal path: one integer comparison.

- Marshaling in `ApplyResultStrategy` just to check would double the marshal on
  every call. The executor also cannot see the whole payload: the strict
  output-schema step keys and the `isError` flag live in the manager.
- A cheaper proxy (item count before serialization) fails for the reason above.
- The over-cap path does extra work (one marshal per row, a few exact re-measures,
  one re-validation), but only for a result already too large to send, which is rare.
- The logic lives in the composite package (`composite.BoundResponse`); the manager
  reaches it through an optional interface, `tools.ResponseBounder`, that
  `CompositeToolHandler` implements. Native tools do not implement it.

### 3. Scope: composite tools, both strategies

Decision: **every composite tool, `collect` and `postProcess` alike,** because
the over-cap data is in step `data` arrays in both strategies (the incident's
72 KB was raw rows; `list-clients` at the ceiling is 318 KB of the same shape).
Write tools implement the interface too, but never reach the cap (a result is one
object); the manager skips the valve for error results.

**Native tools are not covered, and are not counted** in `mcp_response_bounded_total`:
`describe-semp-schema`, `list-brokers`, `get-broker-status`, `get-discard-stats`,
`get-redundancy-status` and `get-queue-metrics`. Five return small fixed-shape
objects. `describe-semp-schema` is the exception: with `view: "raw"` it can exceed
100k characters by design (SOL-155427) and has no step rows to trim, so the valve
could not help it; its description steers callers to the default view. The
documentation says "composite tool" everywhere the cap is described.

### 4. Enforcement: truncate and say so, never fail

Decision: **remove tail rows, flag the step, count and log it.** Not a hard error,
not a log-only warning.

- A hard error turns a result that is mostly fine into nothing, and the agent
  cannot always act on it. Truncation keeps the useful head of the data.
- A log-only warning leaves the host overflow in place, which is the incident.
- Which parts are rows: a step's top-level `data` array. For a `postProcess` tool the
  top-level `summary` is the handler's output, not a step, and is never touched
  even if it contains a `data` array; for a `collect` tool every key is a step,
  including one named `summary` (the loader reserves that step ID only for
  `postProcess`). The tool's result strategy decides which reading applies.
- How: rows come off the end of the **largest** `data` array first, one at a time,
  so arrays shrink toward equal size; ties break on step ID. The same input always
  gives the same output. A first pass estimates from row sizes; the result is then
  built and measured exactly, and rows are removed until it fits, so the text lands
  under the cap within one row of it and no space is reserved for the keys that are
  added. Step maps are copied, not edited in place.
- Whether trimming can help is decided on the **exact** size of the result with every
  array emptied and the real metadata added, not an estimate. (The first version
  compared an estimate that included a per-step reserve and could declare a fixable
  result unfixable.)
- A step with every row omitted keeps `"data": []`, never `null`.
- What the agent sees, on each step that lost rows: `omittedItems: N` (present only
  when the size cap trimmed the step), `truncated: true`, and a `truncatedMessage`
  that states kept/total, the cap, whether the summary is still complete, and how to
  narrow the query. The text contains counts and the cap only, never a parameter
  value or a row.
- An earlier note on the step is kept, with one exception. When `maxResults` cut the
  step short, the fetch writes "Results limited to N. Use maxResults (up to 500) to
  retrieve more." After a size trim that advice is wrong (a larger `maxResults` is
  trimmed back to the same rows), so the valve removes that sentence and keeps the
  count. The ceiling note and the page-limit note are still true and are kept whole.
  The advice text is defined once (`maxResultsAdvice`) and used by both the fetch and
  the valve, so the two cannot drift.
- A `postProcess` summary is computed before trimming and is never touched, so its
  counts still describe every row fetched.
- If emptying every array still would not fit (the excess is in a summary), nothing
  is changed: degrading the result for no gain is worse. It is reported as
  `unbounded`, see Failure modes.
- A trimmed result is validated against the output schema again. A tool whose schema
  forbids the added keys fails loudly rather than sending an invalid result. The
  strict schema for a paginated step declares `omittedItems`, so a strict-schema tool
  accepts a trimmed step.
- Observability: counter `mcp_response_bounded_total{tool,broker,outcome}` with
  `outcome` in `truncated`, `unbounded` (metrics schema 1.10 to 1.11), and one WARN
  log line with counts only (byte fields are the indented text length). No new error
  type: the call still succeeds, so the closed `error_type` set and the audit drift
  tests are untouched.

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
  fails if any summary exceeds a quarter of the cap.
  `TestShippedKafkaStatusSummaries_ExerciseTheFailureDetail` pins that the
  worst-case runs actually produce the failure detail the Kafka status handlers cap,
  so removing that cap cannot pass unnoticed. A new tool is covered the moment it is
  in `tools.yaml`.
- Both layers are needed: the runtime valve covers what the test cannot see
  (real data wider than the model), and the test covers what the valve cannot
  trim (a summary). Neither alone is enough.

Mutation checks, run and recorded: removing the `topOffenderLimit` cap in
`list_queue_discards.go`, or `maxFailedBindingsListed` in
`get_kafka_bridge_status.go`, fails the drift tests (the second passed under the
first version's generator, which never produced a down binding); every behavior of
`BoundResponse` and its manager wiring (inclusive boundary, exact measurement,
largest-first selection, similar steps shrinking together, `omittedItems`, exact
feasibility, empty arrays, summary exclusion, advice removal, error-result skip,
metric, re-validation, outcome label, the text gate) fails at least one test when
removed.

## Alternatives considered

| Alternative | Why it lost |
|---|---|
| Item-count ceiling | Already exists (500); does not capture row width |
| Token estimate | Host-specific; the server cannot know it; adds a heuristic that looks precise and is not |
| Cap on the compact structured copy | The text is the larger copy; trimmed results reached the model at up to 1.47x the cap |
| Fixed indent factor, or a lower cap | One factor over-trims tight shapes by up to a third; the exact text size is already available |
| Hard error over the cap | Loses data the agent could have used; not actionable for tools with no paging |
| Log-only warning | Does not prevent the overflow |
| Check in `ApplyResultStrategy` | Needs its own marshal on every call; cannot see the output schema or `isError` |
| Per-tool `maxBytes` in YAML | A number per tool, unverifiable, drifts; repeats the bespoke-flag problem |
| Configurable cap (config key, Helm value) | See below |
| Trim summary values too | A summary is semantic (counts, groupings); dropping keys silently makes figures wrong |
| Boot-time lint | See question 5 |
| Fix the value-keyed summaries now | See "Found, not changed" |

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
| Result whose text is at or under 64 KiB (every tool at default except the two below, with realistic data) | None: byte-identical output, no new field |
| `list-vpns` with ~100 VPNs, `get-rdp-status` with ~100 bindings and ~100 consumers | Rows omitted (28 and 60 in the synthetic run), step flagged, summary intact |
| Any list tool at `maxResults` 500 with wide rows | Rows trimmed to fit; step flagged with `omittedItems` |
| `list-queue-discards` | None (summary only, 2.3–3.3 KB) |
| Output shape | Unchanged for every result under the cap. Over the cap, existing keys keep their meaning; `omittedItems` is new, `truncated` / `truncatedMessage` are existing keys, and a `maxResults` advice sentence is dropped from `truncatedMessage` |

This is not marked BREAKING: no field is renamed or removed, and a result is only
changed when it is larger than a size that is at or below the one that failed a host
in production. A caller that relied on receiving all 500 wide rows of one call will
now receive fewer, and is told so. That is called out in the changelog and the PR so
it can be challenged.

## Failure modes

- **Summary too large** (`unbounded`). The valve cannot trim a summary, so a handler
  whose summary grows with the data is sent as built, counted as `outcome=unbounded`
  and logged. The CI tests are the first defence; the counter is the second.
- **A caller wants the omitted rows.** Most list tools have no cursor, so they are
  not reachable through that tool call: the message says to narrow the query. This
  is an existing limit of the tools (the 500 ceiling), now stated rather than silent.
- **Cap too low or high for a given host.** The text cap is conservative against the
  one known failure under either reading of its unit; a host with a much smaller
  budget still needs a smaller `maxResults`.
- **Extra work on the over-cap path.** One marshal per row, a few exact measures of
  the whole result, and a re-validation; paid only by an already-oversized result.
- **Schema rejects the added keys.** Fails as an output-validation error, visibly.
  Not reachable for shipped tools (monitor tools use the permissive envelope, and
  the strict paginated-step schema declares `omittedItems`).
- **Uncapped native tools.** `describe-semp-schema` with `view: "raw"` is not
  covered and is not counted; do not read `mcp_response_bounded_total` as a
  server-wide oversize signal.

## Found, not changed

Measuring turned up a gap in the convention itself. Six postProcess handlers return
maps **keyed by a data value** (`byFailureReason`, `byInboundFailureReason`,
`byLastFailureReason`, `byBindingFailureReason`, `byClientProfile`, `byPlatform`):
their size is bounded by the number of *distinct* values, not by item count. With a
small vocabulary (how brokers behave) summaries stayed under 13.6 KB at the ceiling,
including the failure-heavy runs. If every row carries a unique 200-character reason,
four tools (`list-bridges`, `list-rdps`, `list-kafka-receivers`, `list-kafka-senders`)
reach about 105 KB of summary at 500 rows, which is `unbounded`. That needs 500
objects with distinct reasons; brokers hold far fewer bridges and Kafka objects, so
this is theoretical today.

It is not fixed here: it would change the output of several tools for a case that
does not occur, and the right bound (top-N by count plus an "other" count) is a
product choice. It is recorded here and in the PR as a follow-up.

## Not changed

- Native tools (see question 3).
- Tool descriptions: unchanged. Adding a sentence to ~20 descriptions would cost
  tool-list tokens on every `tools/list` for a rare case; the result carries its own
  explanation. The cap is documented in `docs/tools-reference.md`.
- No paging cursor for list tools.

## Revisions after review (2026-10-23)

The first version measured the compact copy, estimated sizes with a per-step
reserve, and trimmed any top-level key with a `data` array. Review found, and a test
reproduced against that version, each of:

- the cap was measured on the compact copy while the model reads the larger text;
- a `postProcess` summary holding a `data` array was trimmed (100 rows cut to 32);
- a fixable result was reported `unbounded` (in the reproduction, a result 426
  bytes over the cap was sent unchanged although emptying its one row fits);
- a step with every row omitted serialized as `"data": null`;
- the size-test generator never produced a down Kafka binding, so removing the
  failure-detail cap went unnoticed;
- the "largest first" selection was untested (always taking the first step passed);
- a trimmed step still told the agent to raise `maxResults`;
- the docs said "a tool result" where only composite tools are capped.

Each is fixed and covered by a test that fails against the earlier behavior.
