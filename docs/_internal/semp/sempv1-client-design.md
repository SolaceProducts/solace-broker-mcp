# SEMPv1 Client — Design Spec

**Status:** Design proposal
**Scope:** Story 4 (SEMP API Client Foundation) — SEMPv1 portion only
**Related:** Stories 8–12 (tools that will consume this client)

---

## 1. Purpose

Introduce a SEMPv1 client to the `solace-broker-mcp` server so that curated tools
can query broker capabilities not available via SEMPv2 (public or private).
Examples from Stories 8–12: CPU/memory/disk usage, redundancy status, full
discard-stats breakdown.

The SEMPv1 client sits **alongside** the existing SEMPv2 client — not as a
fallback, not as a wrapper, but as a peer protocol that individual tools can
choose to call. Some tools will use only v2, some will use only v1, some may
use both.

---

## 2. Goals and Non-Goals

### Goals
- Loosely coupled to the rest of the codebase (tools choose v1 or v2 independently)
- Easy to test (envelope parsing is unit-testable without HTTP)
- Easy to extend (room for future fields, error kinds, retry decorators)
- Easy to maintain (single responsibility per file; no cross-concern leakage)
- Consistent with the existing SEMPv2 client patterns where possible

### Non-Goals (deferred)
- Rate limiting and retries (Story 5) — add a TODO comment only
- `more-cookie` / SEMPv1 pagination — not needed for MVP commands
- Generic MCP tools like `sempv1_execute` (explicitly excluded; all tools are curated)
- CLI-to-XML conversion (`clitosemp` prototype approach) — explicitly excluded
- Integration test wiring — parked for later

---

## 3. Key Findings That Shape the Design

These came from reading the prototype, the Solace XSD, the live broker
(`solace` Docker container, version 10.25.0.208), and Solace docs.

### 3.1 SEMPv1 supports both Basic and Bearer auth

Confirmed from Solace docs:
> "Legacy SEMP uses Basic Authentication... OAuth authentication is also
> supported using one or more OAuth tokens in the HTTP Authorization header as
> a bearer token."

**Implication:** the v1 client reuses the existing `BrokerConfig.Auth` struct
and mirrors the v2 client's `addAuth()` logic. No new config surface.

### 3.2 Envelope errors arrive as HTTP 200

Confirmed against the live broker:
- HTTP non-2xx only occurs for transport/auth issues (401, 403, 5xx)
- All broker-level failures (parse, permission, limit, execute-fail) come back
  as HTTP 200 with an error element inside `<rpc-reply>`

**Implication:** a client that only inspects HTTP status misses most real
failures. The client **must** peek inside the envelope to distinguish success
from failure.

### 3.3 Four distinct envelope error shapes

| Element | Payload | Example trigger |
|---|---|---|
| `<parse-error>msg</parse-error>` | text only | Unknown command, malformed XML, schema validation failure |
| `<permission-error>msg</permission-error>` | text only | User role lacks privilege for the command |
| `<limit-error>msg</limit-error>` | text only | Response exceeds broker buffer (for example, "response too big: use sequenced get") |
| `<execute-result code="fail" reason="..." reasonCode="..."/>` | attributes | Config/admin command passed parse but failed execution |

Successful commands always include `<execute-result code="ok"/>` at the tail
of `<rpc-reply>` (observed on broker 10.25; contradicts the prototype's
heuristic which treated absence of `execute-result` as success).

**Implication:** the client detects errors by the **presence of any error
element**, not by the absence of a success marker.

### 3.4 Broker-emitted error text is often generic

Example: the live broker returns `"invalid message: schema validation error"`
for most parse failures regardless of what was actually wrong. This limits how
much user-friendly translation the tool layer can do — we surface the broker's
text as-is and add tool-level context.

---

## 4. Package Layout

```
internal/semp/
├── broker.go                  // existing — add sempV1 peer field
├── pool.go                    // existing — add GetSEMPv1(alias)
├── sempv2/                    // existing
│   ├── client.go
│   └── operation.go
└── sempv1/                    // NEW
    ├── client.go              // Client interface, HTTPClient, Execute()
    ├── client_test.go         // httptest-based transport + response-classification tests
    ├── envelope.go            // parseReply() — XML envelope inspection
    └── envelope_test.go       // pure-function tests for all 4 error shapes + success

Note: v1 gets its own `Error` type and `ErrorKind` enum in `sempv1/errors.go`.
The v2 `SEMPError` in `sempv2/client.go` is left untouched (see §5.1 rationale
and drift D7).
```

### 4.1 Why this layout

- **Mirrors `sempv2/`**: consistent mental model for anyone jumping between protocols
- **`envelope.go` separate from `client.go`**: envelope parsing is pure, testable without HTTP; transport is testable without XML knowledge. Split files enforce the boundary.
- **No sub-package for envelope**: the code is small and only used by the client; a package boundary would add indirection with no benefit.

---

## 5. Error Model

### 5.1 Decision: separate error types per protocol

Each protocol owns its own error type in its own package:
- v2 keeps the existing `sempv2.SEMPError` — **no changes**
- v1 introduces `sempv1.Error` with a v1-specific `ErrorKind` enum

Rationale (reversed from an earlier shared-struct proposal — see drift D7):

- **18 of 22 planned tools are v2-only** (Stories 9, 10, 11, 12, 14, 15, 16).
  Only 2 tools are v1-only (`get-broker-status`, `get-redundancy-status`) and
  2 tools are mixed. The "one shared struct helps cross-protocol callers"
  argument was theoretical; in practice tools know which protocol they call.
- **A shared struct forces semantic gymnastics:** `StatusCode == 200 means
  error` (v1 envelope case) vs `StatusCode == real HTTP status` (v2 case) is a
  readable-but-subtle trap. Separate types enforce the distinction at the
  type level.
- **Q-004's translation table (Story 13B) is HTTP-status-driven** and built
  for v2. A v1 envelope error (HTTP 200 + `<parse-error>`) has no entry in
  that table. 13B will use a type switch over `*sempv2.SEMPError` and
  `*sempv1.Error` to branch on the correct axis for each protocol.
- **Zero regression surface for v2.** No existing call sites in
  `sempv2/client.go`, `registry.go`, or the composite executor tests need
  to change.

### 5.2 Types

v1's errors live in a new file `internal/semp/sempv1/errors.go`:

```go
package sempv1

// ErrorKind classifies a SEMPv1 failure so callers can branch without parsing
// the raw body. A zero-valued Kind (ErrorKindUnknown) indicates a malformed
// or unclassified response.
type ErrorKind int

const (
    ErrorKindUnknown     ErrorKind = iota // zero value; malformed envelope or unclassified
    ErrorKindHTTP                         // HTTP-layer failure (401, 403, 404, 5xx)
    ErrorKindParse                        // <parse-error> in envelope
    ErrorKindPermission                   // <permission-error> in envelope
    ErrorKindLimit                        // <limit-error> in envelope
    ErrorKindExecuteFail                  // <execute-result code="fail">
)

// Error is returned for any SEMPv1 failure — either an HTTP-layer error
// (non-2xx response) or an envelope-layer error (HTTP 200 + error element
// inside <rpc-reply>). Callers branch on Kind.
//
// Field semantics by Kind:
//   - Kind == ErrorKindHTTP: StatusCode is the real HTTP status; Message is
//     empty; ReasonCode is zero; Body holds the raw response.
//   - Kind == ErrorKindExecuteFail: StatusCode is 200; Message is the reason
//     attribute; ReasonCode is the reasonCode attribute.
//   - Kind == ErrorKindParse / Permission / Limit: StatusCode is 200;
//     Message is the text content of the error element; ReasonCode is zero.
//   - Kind == ErrorKindUnknown: response did not match any known shape;
//     Body holds the raw bytes for debugging.
//
// Body is always preserved as a safety net for information the client did
// not structurally extract.
type Error struct {
    Kind       ErrorKind
    StatusCode int
    Message    string
    ReasonCode int
    Body       []byte
}

func (e *Error) Error() string { /* ... */ }
```

### 5.3 When each Kind is produced

| Kind | Source | StatusCode | Message | ReasonCode |
|---|---|---|---|---|
| `ErrorKindHTTP` | non-2xx HTTP response | real status (401, 403, 404, 5xx) | empty | 0 |
| `ErrorKindParse` | `<parse-error>text</parse-error>` | 200 | element text | 0 |
| `ErrorKindPermission` | `<permission-error>text</permission-error>` | 200 | element text | 0 |
| `ErrorKindLimit` | `<limit-error>text</limit-error>` | 200 | element text | 0 |
| `ErrorKindExecuteFail` | `<execute-result code="fail" reason="..." reasonCode="..."/>` | 200 | `reason` attr | `reasonCode` attr |
| `ErrorKindUnknown` | malformed XML, missing `<rpc>` on success | 200 or real status | "empty reply" or empty | 0 |

### 5.4 What the client does NOT do with the error

Per §7.2 (Client Boundary), the client classifies and returns. It does not:
- Retry (Story 5 decorator's job)
- Translate to user-facing MCP messages (Story 13B's job)
- Decide which errors are recoverable (tool's job, informed by Kind)

Callers above the client make those decisions using `errors.As` on `*sempv1.Error`.

---

## 6. Client Interface

### 6.1 Interface

```go
package sempv1

// Client executes SEMPv1 XML commands against a Solace broker's /SEMP endpoint.
// Implementations: HTTPClient (real), mock (tests).
// Future: rate-limited or retry decorators wrapping this interface (Story 5).
type Client interface {
    Execute(ctx context.Context, xml string) (*Result, error)
}

// Result holds a successful SEMPv1 response payload.
// On success, InnerXML contains only the <rpc>...</rpc> inner bytes —
// callers never see <rpc-reply>, <execute-result>, or any error envelope.
type Result struct {
    InnerXML []byte
    // Room to grow: SEMPVersion string (from rpc-reply attribute), etc.
}
```

### 6.2 Why `(*Result, error)` instead of `(string, error)`

- Matches the v2 client's `Execute(ctx, op, args) (*Result, error)` shape
- Bytes naturally represent XML — avoids needless string conversion
- Extensible without breaking callers

Story 4 suggests `(string, error)`; we deviate with intent (documented in
§3.2 of the story discussions) for consistency and future-proofing.

### 6.3 `context.Context` is required

- Enables per-call timeouts and cancellation
- Already the Go idiom and matches v2 client
- Story omits it; we add it deliberately

---

## 7. Client Responsibility Boundary

### 7.1 Client DOES

1. Build HTTP POST to `{broker_url}/SEMP`
2. Set `Content-Type: application/xml`
3. Apply auth (Basic or Bearer, per `BrokerConfig.Auth.Mode`)
4. Send request and read raw body
5. Handle HTTP-layer failures → return `*sempv1.Error{Kind: ErrorKindHTTP, ...}`
6. Parse the envelope:
   - Detect any of the 4 error elements → return `*sempv1.Error{Kind: ..., StatusCode: 200, ...}`
   - On success, extract inner `<rpc>` XML → return `*Result`
7. Minimal input validation:
   - `ctx != nil`
   - `xml` string non-empty
   - Broker URL + auth configured (checked at construction, not per call)

### 7.2 Client does NOT

- Parse the `<rpc>` payload structure (tool's job)
- Validate command-specific fields (tool's job)
- Translate broker errors to user-friendly MCP messages (tool's job — Q-004)
- Retry or rate-limit (Story 5 — decorator layer)
- Know anything tool-specific
- Build XML requests (tool's job — see §9)

### 7.3 Why this boundary

- Keeps the client **reusable** — any tool sends any v1 command
- Keeps the client **testable** — no domain mocks needed
- **Single responsibility** — "send XML, return inner XML or typed error"
- Envelope knowledge lives in one place — tools don't re-implement the same
  parse-error/permission-error/limit-error/execute-result checks

---

## 8. Envelope Parsing (`envelope.go`)

### 8.1 Contract

```go
package sempv1

// parseReply inspects a raw <rpc-reply>...</rpc-reply> body and returns either
// the inner <rpc> XML bytes (success) or a classified *Error (failure).
// The caller is responsible for HTTP-level status checks; parseReply is only
// called when HTTP status is 2xx.
func parseReply(body []byte) ([]byte, *Error)
```

Only one exported-to-package function. Client never touches `<rpc-reply>`,
`<parse-error>`, `<execute-result>`, etc. directly.

### 8.2 Detection logic

```
1. Unmarshal body into an rpcReply struct that models:
     <rpc-reply semp-version="...">
       <rpc>...</rpc>?
       <more-cookie>...</more-cookie>?
       <execute-result code=".." reason=".." reasonCode="..."/>?
       <parse-error>...</parse-error>?
       <permission-error>...</permission-error>?
       <limit-error>...</limit-error>?
     </rpc-reply>

2. Check in priority order (most specific first):
     a. parse-error      → Error{Kind: ErrorKindParse, Message: text}
     b. permission-error → Error{Kind: ErrorKindPermission, Message: text}
     c. limit-error      → Error{Kind: ErrorKindLimit, Message: text}
     d. execute-result code="fail" → Error{Kind: ErrorKindExecuteFail,
                                            Message: reason,
                                            ReasonCode: reasonCode}

3. Otherwise:
     Return inner bytes of <rpc> element. If <rpc> is absent, treat as
     malformed and return Error{Kind: ErrorKindUnknown, Message: "empty reply"}.
```

### 8.3 Known broker quirk

Broker 10.25 always emits `<execute-result code="ok"/>` on success — for both
show and admin commands. The prototype's heuristic ("no execute-result = show
command succeeded") is incorrect for this version. Our logic does not rely on
that pattern.

### 8.4 Envelope unit tests (`envelope_test.go`)

Pure-function tests on `parseReply()` — raw XML bytes in, outcome asserted.
No HTTP involved.

Fixtures:
- `<rpc-reply><rpc><show><version/></show></rpc><execute-result code="ok"/></rpc-reply>` → success, inner `<show>...</show>` bytes extracted
- `<rpc-reply><parse-error>invalid message</parse-error></rpc-reply>` → `Kind: Parse`, `Message: "invalid message"`
- `<rpc-reply><permission-error>not authorized</permission-error></rpc-reply>` → `Kind: Permission`, `Message: "not authorized"`
- `<rpc-reply><limit-error>response too big</limit-error></rpc-reply>` → `Kind: Limit`, `Message: "response too big"`
- `<rpc-reply><execute-result code="fail" reason="foo" reasonCode="431"/></rpc-reply>` → `Kind: ExecuteFail`, `Message: "foo"`, `ReasonCode: 431`
- Malformed XML (unclosed tag) → `Kind: Unknown`
- Empty `<rpc-reply/>` → `Kind: Unknown`, `Message: "empty reply"`

### 8.5 Client-level HTTP unit tests (`client_test.go`)

Integration of transport + envelope parsing using `httptest.Server` to mock
the broker. Verifies request construction and response classification end-to-end
without touching a real broker.

**Request construction:**
- Auth header: Basic mode → `Authorization: Basic base64(user:pass)`
- Auth header: Bearer mode → `Authorization: Bearer <token>`
- `Content-Type: application/xml` is set
- POST body equals the caller-supplied XML string
- URL path is `{baseURL}/SEMP`
- `context.Context` cancellation aborts the in-flight request

**Response classification — HTTP status cases (story DoD line 1151):**
- Mock returns 401 → `*sempv1.Error{Kind: ErrorKindHTTP, StatusCode: 401, Body: <raw>}`
- Mock returns 403 → `*sempv1.Error{Kind: ErrorKindHTTP, StatusCode: 403, Body: <raw>}`
- Mock returns 404 → `*sempv1.Error{Kind: ErrorKindHTTP, StatusCode: 404, Body: <raw>}`
- Mock returns 500 → `*sempv1.Error{Kind: ErrorKindHTTP, StatusCode: 500, Body: <raw>}`
- Mock returns 2xx + network error mid-read → wrapped transport error (not a `*sempv1.Error`)

**Response classification — HTTP 200 cases (envelope delegation):**
- Mock returns 200 + valid success envelope → `*Result{InnerXML: <bytes>}`
- Mock returns 200 + `<parse-error>` envelope → `*sempv1.Error{Kind: ErrorKindParse, StatusCode: 200}`
- Mock returns 200 + `<permission-error>` → `*sempv1.Error{Kind: ErrorKindPermission, StatusCode: 200}`
- Mock returns 200 + `<limit-error>` → `*sempv1.Error{Kind: ErrorKindLimit, StatusCode: 200}`
- Mock returns 200 + `<execute-result code="fail" .../>` → `*sempv1.Error{Kind: ErrorKindExecuteFail, ReasonCode: <n>, StatusCode: 200}`

Parsing logic itself is covered by `envelope_test.go`; client tests exercise
the transport path and assert the error types surface correctly.

### 8.6 Library choice

Use stdlib `encoding/xml` for all envelope parsing. No third-party XML
dependencies. Matches Story 4 Technical Notes line 1145 and keeps the binary
dependency graph small (consistent with v2's stdlib `encoding/json` choice).

---

## 9. XML Request Construction (Convention for Stories 8–12)

### 9.1 Who builds XML

**The tool, not the client.** The client accepts an opaque XML string.

```
internal/tools/get-redundancy-status.go  ← owns buildShowRedundancyXML()
internal/tools/get-broker-status.go      ← owns buildShowVersionXML() etc.
internal/semp/sempv1/                    ← knows nothing about commands
```

### 9.2 Representation

Hand-written string templates, not `encoding/xml` structs.

Rationale:
- All MVP commands are 1-liners
- Matches the XSD / docs 1:1 — easy to verify against spec
- `encoding/xml` structs are 5–10× the code for identical output
- The prototype uses strings; so do the stories

### 9.3 Safety rules (MANDATORY for any dynamic value)

All user-supplied or externally-sourced values MUST pass through escaping
before being embedded in XML:

```go
// in a shared helper package (e.g. internal/semp/sempv1/xml_util.go)
func Escape(s string) string {
    var buf bytes.Buffer
    xml.EscapeText(&buf, []byte(s))
    return buf.String()
}
```

Pattern for builders:

```go
func buildShowQueueXML(vpnName, queueName string) string {
    return fmt.Sprintf(
        `<rpc><show><queue><name>%s</name><vpn-name>%s</vpn-name></queue></show></rpc>`,
        Escape(queueName),
        Escape(vpnName),
    )
}
```

### 9.4 Defense in depth — input validation BEFORE XML

Tools validate semantic rules before the XML layer:
- VPN name: `[a-zA-Z0-9_-]+`, max 32 chars
- Queue name: same class, max 200 chars
- Numeric IDs: parse to int, reject on fail

Escape protects XML syntax; validation protects the broker from known-bad input.

### 9.5 Hostile-input tests

Each builder must have a unit test with values containing `<`, `>`, `&`, `'`,
`"` and verify the output is a valid well-formed XML string with entity-encoded
values.

---

## 10. Integration with the Broker Pool

### 10.1 Decision: peer clients, not a wrapping `SEMPClient`

Story 4 suggests:
```go
type SEMPClient struct {
    v1 SEMPv1Executor
    v2 SEMPv2Executor
}
```

We reject this in favor of peer fields on the existing `BrokerClient`:

```go
// internal/semp/broker.go (modified)
type BrokerClient struct {
    sempV1 sempv1.Client
    sempV2 sempv2.Client
    alias  string
}

func (b *BrokerClient) SEMPv1() sempv1.Client { return b.sempV1 }
func (b *BrokerClient) SEMPv2() sempv2.Client { return b.sempV2 }
```

Why:
- `SEMPClient` in the story is a type with no behavior — just holds two pointers
- Peer fields mirror the current pattern (zero refactor churn)
- Story 4 explicitly says v1/v2 are used **simultaneously, not swappable** — there's no fallback logic that would need a shared home
- YAGNI — if cross-protocol orchestration ever becomes a thing, we add a thin wrapper then

### 10.2 Pool changes

```go
// internal/semp/pool.go (modified)
func (p *BrokerPool) GetSEMPv1(alias string) (sempv1.Client, error) { /* ... */ }
func (p *BrokerPool) GetSEMPv2(alias string) (sempv2.Client, error) { /* ... */ }
```

Both use the same lazy-creation, double-checked-locking pattern that exists
today. `NewBrokerClient` initializes both protocol clients from the same
`BrokerConfig` + `SEMPConfig`.

### 10.3 Config reuse

v1 and v2 share `BrokerConfig.URL`, `Auth`, `InsecureSkipVerify`, and
`SEMPConfig.RequestTimeoutDuration`. No new config surface is introduced.

---

## 11. Deferred Work (Tracked)

### 11.1 Rate limiting + retries (Story 5)

Add a TODO comment at the top of `sempv1/client.go`:
```go
// TODO(Story 5): wrap with rate-limiting + retry decorator (shared with v2).
// Current implementation is the transport layer only; resilience lives in a
// separate layer so both v1 and v2 benefit without duplication.
```

### 11.2 Pagination (`<more-cookie>`)

Not needed for MVP commands (Stories 8–12). If a future command paginates,
extend `Result` with cursor handling or add an `ExecuteAll(ctx, xml)` helper.

### 11.3 Integration test wiring

Parked. When we resume: likely `//go:build integration` tag + skip-if-unreachable
against the local Docker `solace` container (`localhost:8081`). To confirm:
whether the repo already has an integration-test pattern for v2 that we'd match.

---

## 12. Pilot Tool (Phase 2, after Foundation)

Phase 1 of this work is the client foundation only (this document). Phase 2
wires a pilot tool end-to-end on top of the foundation to prove it works.

Candidate: `get-redundancy-status`. Rationale:
- Single v1 call — smallest surface that exercises the full path
- Story 8 explicitly calls this out as SEMPv1-only
- Simple XML payload, small response, no pagination concerns

`get-broker-status` is richer (4 parallel calls) but introduces concurrency
concerns that belong in a separate follow-up.

**Decision deferred** — revisit after Phase 1 lands.

---

## 13. Deliberate Drifts from Story 4

This section consolidates every place this design deviates from what Story 4
literally specifies. Each drift was discussed and approved; this is not a
silent deviation log. Reviewers checking the spec against the story's
acceptance criteria should read this section first.

| # | Story 4 literal text | Spec position | Reason for drift | Spec ref |
|---|---|---|---|---|
| D1 | `Execute(request string) (string, error)` (line 1112) | `Execute(ctx context.Context, xml string) (*Result, error)` | (a) Matches existing v2 client's `Execute` shape for consistent mental model. (b) `*Result` leaves room to add fields (for example, `SEMPVersion`) without breaking callers. (c) Bytes represent XML naturally. | §6.1, §6.2 |
| D2 | No `context.Context` in v1 signature (line 1112) | `ctx` is required | (a) Standard Go idiom. (b) Matches v2 client. (c) Needed for per-call timeout + cancellation. | §6.3 |
| D3 | `SEMPClient { v1, v2 }` container struct (line 1113–1119) | Peer fields `sempV1` + `sempV2` directly on existing `BrokerClient` | (a) The proposed wrapper has no behavior — just groups two pointers. (b) Peer pattern matches the current codebase (zero refactor churn). (c) Story itself says v1/v2 are used "simultaneously, not swappable" — no cross-protocol logic needs a shared home. (d) YAGNI: if orchestration becomes a thing, add a wrapper then. | §10.1 |
| D4 | Interface file at `internal/semp/v1executor.go` (line 1111) | Interface in `internal/semp/sempv1/client.go` | Mirrors the existing `sempv2/` package layout. Minor file-layout choice, same effective public surface. | §4 |
| D5 | Work item 12 + DoD: "Write integration tests… against live broker" (lines 1134, 1161) | Parked to a separate follow-up | (a) User explicitly requested deferring integration-test wiring to focus on foundation. (b) Repo's existing integration-test conventions for v2 need confirmation before we adopt a pattern for v1. | §11.3, §15 |
| D6 | Shared `SEMPError` struct spec is silent for v1 | Extended with `Kind`, `Message`, `ReasonCode` shared with v2 | Story's line 1087 ("Include original SEMP error details in MCP error response") and Q-004's translation strategy both require structured error classification. Single shared struct avoids two parallel error taxonomies. | §5 |
| D7 | **SUPERSEDES D6.** Earlier spec version §5.1 proposed a shared `SEMPError` across v1 and v2 | Separate types: `sempv2.SEMPError` (unchanged) + new `sempv1.Error` | (a) Empirical tool distribution (18 v2-only, 2 v1-only, 2 mixed) — shared struct helps almost nobody. (b) Shared struct required `StatusCode == 200 means envelope error` semantic trap. (c) Q-004's HTTP-status-driven translation table doesn't fit v1 envelope errors; Story 13B needs a type switch anyway. (d) Zero regression surface for existing v2 code. | §5 |

### What is NOT a drift

For clarity, these are places where the spec fully aligns with Story 4 — not
drifts, not gaps:

- "Both SEMP v1 and v2 used simultaneously, not swappable" (line 1100) → peer
  clients satisfy this exactly (§10.1)
- "XML request/response handling" (line 1076) → `Execute` + `envelope.go` (§6, §8)
- "XML parsing: Use stdlib `encoding/xml`" (line 1145) → explicit in §8.6
- "Unit tests: mock XML responses" (line 1101) → covered in §8.4
- "Mock error responses (401, 404, 500)" (line 1151) → explicit fixtures in §8.5
- "Resource/Tool code can call either or both as needed" (line 1099) → tools
  access `brokerClient.SEMPv1()` and/or `SEMPv2()` independently (§10.2)

---

## 14. Acceptance — Story 4 DoD mapping

| Story 4 DoD item | How this design addresses it | Drift? |
|---|---|---|
| Code complete and peer reviewed | Via normal PR flow | — |
| Unit tests passing for SEMPv1 | `client_test.go` (§8.5) + `envelope_test.go` (§8.4) | — |
| Integration tests succeed against live broker | Deferred to follow-up | **D5** |
| XML parsing works for SEMPv1 responses | `envelope.go` handles all 4 error shapes + success (§8) | — |
| Error responses parsed correctly | `sempv1.Error{Kind, Message, ReasonCode}` covers HTTP + all envelope shapes (§5); v2 `SEMPError` unchanged | **D7** (separate types) |
| Both SEMP v1 and v2 can be used simultaneously | Peer clients on `BrokerClient`; independent pool accessors (§10) | **D3** (no wrapper struct) |
| No new compiler warnings | Enforced via existing `.golangci.yml` in CI | — |

---

## 15. Open Questions (from discussion — none blocking)

| # | Question | Current plan |
|---|---|---|
| Rate limit | Where does the retry decorator live? | Story 5; leave TODO comment |
| Pilot choice | `get-redundancy-status` versus `get-broker-status`? | Decide after Phase 1 |
| Integration tests | Build tag + skip, or always-on with Docker? | Parked |

---

## 16. References

- **Story 4** (SEMP API Client Foundation) — `story-review/stories.md` lines 1042–1168
- **Q-004** (Tool Error Handling Strategy) — `story-review/stories.md` lines 65–110
- **Stories 8–12** (consumers of this client) — `story-review/stories.md` lines 1414+
- **Prototype reference** — `broker-mcp/go-sempv1/internal/sempv1/` (client, envelope, response)
- **Existing v2 client** — `internal/semp/sempv2/client.go`
- **Existing broker pool** — `internal/semp/pool.go`, `internal/semp/broker.go`
- **Solace docs**
  - [Using Legacy SEMP](https://docs.solace.com/Admin/SEMP/Using-Legacy-SEMP.htm)
  - [SEMP Error Handling](https://docs.solace.com/Admin/SEMP/SEMP-Error-Handling.htm)
  - [SEMP Authentication and Authorization](https://docs.solace.com/Admin/SEMP/SEMP-Security.htm)
- **Authoritative XSD** — `broker-mcp/go-sempv1/internal/sempv1/schemas/replySchema.xsd`
- **Live broker used for verification** — Docker `solace` container, Solace PubSub+ Standard 10.25.0.208
