# SOL-149606 — Capture per-user identity in tool-invocation audit logs

**Ticket:** https://sol-jira.atlassian.net/browse/SOL-149606
**Branch:** `amorade/SOL-149606`
**Status:** Plan — awaiting implementation

---

## 1. Problem

Tool-invocation log lines emitted by `internal/tools/manager.go:225-270` record
`tool`, `broker`, `status`, `duration`, and (on error) error/SEMP fields. They
do **not** record *who* invoked the tool.

The OAuth/OIDC verifier at `internal/auth/middleware.go:101-147` validates
JWTs at the HTTP boundary and extracts `sub` into `*sdkauth.TokenInfo`. The
go-sdk's `RequireBearerToken` middleware places that `TokenInfo` on the
request context, and the MCP SDK forwards it to tool handlers via
`req.Extra.TokenInfo` (`mcp/shared.go:481` in `go-sdk@v1.5.0`).

The shim at `internal/tools/register.go:55-61` discards the request after
parsing arguments. Identity never reaches `ToolManager.CallTool`. As a
result, audit logs cannot answer the question:

> *"Who ran what tool against which broker?"*

This ticket wires identity through to the log site and surfaces the fields
needed to make tool-invocation logs answer that question.

## 2. Behavior summary

The four claims we log, and how each auth mode interacts with them:

| Mode | Middleware | TokenInfo | `sub` | `iss` | `client_id` | `jti` |
|---|---|---|---|---|---|---|
| `oauth` | runs | from JWT | `claims.sub`, or `<absent>` if empty | `claims.iss`, or `<absent>` | `claims.client_id`, or `<absent>` | `claims.jti`, or `<absent>` |
| `static` | runs | hardcoded `UserID: "dev-user"` (`middleware.go:93`) | `sub=dev-user` | `iss=<absent>` | `client_id=<absent>` | `jti=<absent>` |
| `disabled` | does **not** run | none | (no field) | (no field) | (no field) | (no field) |

**One sentinel.** All four fields use the same `<absent>` sentinel
when empty — log consumers don't need to learn two strings.

This ticket's job is to **record** the identity present on each tool
call. Operator alarming on empty identity ("your IdP is
misconfigured") is a telemetry concern (SOL-149791's metrics) and a
verifier concern (a future ticket may tighten the verifier to reject
spec-non-compliant tokens — see §8.1). Neither belongs here. The
field value `<absent>` is data; alarming on that data belongs to
operator tooling, not to this audit-logging change.

**Why `iss` is included:** RFC 7519 §4.1.2 — *"The subject value MUST
either be scoped to be locally unique in the context of the issuer or
be globally unique."* `sub` alone is unique only per-issuer. A
deployment that federates two IdPs could produce two identical `sub`
values for two different humans. Including `iss` in every log line
disambiguates this case at zero forensic cost and a tiny line-length
cost. (OIDC Core §2 confirms: *"A locally unique and never reassigned
identifier within the Issuer for the End-User."*)

Distinctions:
- **`<absent>`** = "claim was not present or empty when we built the audit record." Applies uniformly to `sub`, `iss`, `client_id`, `jti` in `oauth` / `static` modes.
- **No field at all** = "auth did not run" (disabled mode). Reader infers from the startup banner that this is not an audit trail.

Schema commitment: in `oauth` and `static` modes, all three fields are **always present** in the log line. Values may be sentinels, but the field names are stable — log consumers (SIEM, grep) can rely on a fixed schema.

## 3. Scope

### 3.1 In scope

1. **Extract identity at the shim.** Read `req.Extra.TokenInfo` in
   `register.go:55` (after nil-checking `req.Extra` itself — see
   §5.5), construct a small repo-owned `Identity` value, and forward
   it into `ToolManager.CallTool`.
2. **Surface the claims we need.** `TokenInfo.UserID` already carries `sub`.
   Add `iss`, `client_id` and `jti` to the claims struct in
   `createOIDCTokenVerifier` (`middleware.go:125-129`) and stash them in
   `TokenInfo.Extra` so the shim can read them.
3. **Emit identity in audit logs.**
   - Success path (`manager.go:227-231`): add `sub`, `iss`, `client_id`, `jti`.
   - Error path (`manager.go:235-269`): same.
   - Destructive-op WARN (`manager.go:164-168`): same — destructive
     operations are the highest-value audit signal.
4. **Defense in depth at the log site.** Normalize any empty claim
   value to the `<absent>` sentinel. See §2 for the full
   mode/sentinel matrix.
5. **Log-injection defense.** Sanitize identity fields (CR/LF/ANSI/length
   cap) before emit, even though `slog.String` JSON-escapes by default.
6. **Tests.** Unit tests for the `Identity` value (`LogValue`, sanitizer,
   `<absent>` sentinel) and one integration-style test that exercises the
   shim → manager → log path with a built `*TokenInfo`.

### 3.2 Out of scope

- **A new audit log stream / sink.** That belongs to SOL-149791
  ([Broker MCP Observability epic
  FD](https://github.com/SolaceDev/discovery/blob/main/Broker-MCP/broker-mcp-obs-tel-sol-149791/sol-149791-FD.md)).
  This ticket enriches the existing `slog` lines; SOL-149791 owns the
  pipeline that exports them.
- **Log retention, redaction, or access controls.** These are operator
  concerns. `sub` is PII under GDPR; ops are responsible for treating
  audit logs accordingly. The PR description will state this explicitly.
- **Adding identity to non-tool log sites** (startup, broker pool init,
  composite-tool internal steps). Tool-invocation lines are the audit
  primitive; other lines do not warrant per-user attribution today.
- **`email` / `preferred_username` / scope / expiration claims.** These
  are either higher-PII (email, username) or have no audit value-add
  (scope, expiration). Omitting now is a deliberate choice. See §6.4.
- **Auth-failure attribution.** When the SDK middleware or the OIDC
  verifier rejects a token (missing/malformed `Authorization` header,
  bad signature, expired, wrong audience, claims-parsing error), no
  `Identity` is constructed and no tool-invocation log line is
  emitted. Identity attribution for rejected requests requires a
  separate HTTP-level access log — source IP, user-agent, rejection
  reason — because the token's claims cannot be trusted once
  validation fails. Extracting `sub` from a tampered token and
  logging it would let an attacker forge audit entries; that is
  strictly worse than no attribution. A different log channel
  (HTTP access log, not claims-derived) is the right answer; out of
  scope here.

### 3.3 Sparks left in place

- **Dev/static-mode log lines say `sub=dev-user`.** They are not a real
  audit trail. Documented as such in the README + startup banner. Not
  rewritten.
- **Disabled-mode log lines have no `sub` field.** No middleware runs;
  no identity exists. Documented; not faked.

## 4. Design principles

These came out of planning. Each is load-bearing for one or more
implementation choices below.

### 4.1 The type system owns the audit-log schema

We do not pass `*sdkauth.TokenInfo` to `slog`. We construct a repo-owned
`Identity` struct that implements `slog.LogValuer` and emits only the
fields we have committed to logging.

**Why (load-bearing reasons, in order of importance):**

1. **`TokenInfo.Extra` is `map[string]any`.** A struct-level
   `slog.Any(t)` dump would serialize whatever the SDK or our verifier
   stashes there. If a future SDK release adds a new `Extra` key (raw
   token, refresh token, sensitive claim), it leaks silently. A
   dedicated type whose fields *are* the audit schema cannot leak
   what isn't a field.

2. **Audit schema as a type.** `Identity` is a policy type, not a
   data carrier. Adding a field to it is a code-review event. Adding
   a field to `TokenInfo.Extra` is invisible to our log site. The
   type system enforces "we log only this list."

3. **Incidentally**, Go's method-set rule (a method must be declared
   in the same package as its receiver type) prevents us from
   attaching `LogValue()` directly to `sdkauth.TokenInfo`. A wrapper
   type (`type loggedTokenInfo sdkauth.TokenInfo`) is technically
   possible but inherits the `Extra map[string]any` leak risk — so it
   does not solve the load-bearing problem.

**Cost:** one construction site (the shim), one mapping unit test, one
comment at the type definition documenting the contract.

### 4.2 Defense in depth at the log site, not just the verifier

The log layer normalizes empty/whitespace claim values to an
`<absent>` sentinel.

**Why:** the verifier today does not enforce claim presence (RFC 7519
§4.1.2 marks `sub` OPTIONAL — *"Use of this claim is OPTIONAL"*).
Even if it did, defense in depth is standard practice (NIST SP
800-160): each layer assumes the others may fail. A `sub=""` field
in a log file looks like a serialization bug; `sub=<absent>` is a
deliberate signal that the audit-log layer saw no value to record.

Alarming on `sub=<absent>` (or counting its occurrence) is the
operator's concern via telemetry tooling (SOL-149791) and the
verifier's concern via future tightening (§8.1) — not this layer's.

**Cost:** a single normalization helper + one test.

### 4.3 Log honesty over uniform shape

We do **not** force every log line to carry an `auth_mode=…` field.
Auth mode is logged once at startup via `auth.LogStartupBanner`. Adding
it to every tool-invocation line would be pure noise in the 99%
prod-OAuth case, in exchange for a marginal forensic signal that
sophisticated readers can already get from the absence of `sub` (in
disabled mode) or its `dev-user` value (in static mode).

The startup banner is the right place for the mode signal. Audit
forensics requires reading both the banner and the lines — that is
acceptable.

### 4.4 Sanitize before logging, even through structured handlers

`slog.String` with the JSON handler escapes control characters, but
the audit pipeline is defense-in-depth: we cap identity field length
and strip CR/LF/ANSI before constructing the `Identity` value.

**Why:** the log-forging attack described by CWE-117 (Improper Output
Neutralization for Logs) is exactly what occurs when an identity
field contains a `\n` followed by a forged log line. JSON escaping
neutralizes it for the JSON handler; sanitization at the field level
neutralizes it for *any* handler (text, custom) and for any
downstream re-emission (error messages, metric labels, panic
strings). `sub` is supplied by the IdP; in client-credentials flows
where the AS uses `client_id` as `sub`, the value is partially
attacker-influenced.

The 256-byte length cap is a defense-in-depth choice, not a spec
requirement. RFC 7519 §4.1.7 specifies `jti` as *"a case-sensitive
string"* with no length limit. Real-world `sub`/`jti` values from
production IdPs are 20–50 chars; the cap fires only on malicious or
buggy IdPs and prevents log-flood DoS. Truncating spec-legal values
is the acknowledged tradeoff.

**Cost:** a small sanitizer (`sanitizeClaim(string) string`) and one
unit test asserting CR/LF/ANSI/length behavior.

## 5. Architecture

### 5.1 The `Identity` type

```go
// Identity carries the audit-relevant subset of OIDC claims for a single
// tool invocation. Adding a field here is a deliberate commitment to
// logging it. TokenInfo (from the SDK) is intentionally NOT logged
// directly — its Extra map could carry sensitive values in the future.
type Identity struct {
    present  bool    // false when no TokenInfo was on the request (disabled mode)
    sub      string  // claims.sub, or "<absent>" if empty
    iss      string  // claims.iss, or "<absent>" if IdP didn't issue
    clientID string  // claims.client_id, or "<absent>" if IdP didn't issue
    jti      string  // claims.jti, or "<absent>" if IdP didn't issue
}

func (i Identity) LogValue() slog.Value {
    if !i.present {
        // Disabled mode — emit no identity attributes at all. The Group
        // value is empty so logs in disabled mode are byte-identical to
        // today's lines.
        return slog.GroupValue()
    }
    return slog.GroupValue(
        slog.String("sub", i.sub),
        slog.String("iss", i.iss),
        slog.String("client_id", i.clientID),
        slog.String("jti", i.jti),
    )
}
```

Notes:
- Fields are unexported. Construction happens only via
  `NewIdentityFromTokenInfo(*sdkauth.TokenInfo) Identity`, which lives
  alongside the type. Single construction point → grep-able audit
  schema. The constructor is responsible for normalizing empty values
  to `<absent>` (for `sub`) or `<absent>` (for `iss`, `client_id`,
  `jti`), and for setting `present` correctly (see §5.5).
- In oauth/static modes (`present == true`) all four fields are
  always emitted. In disabled mode (`present == false`) the
  `LogValue` returns an empty group, suppressing identity attributes
  entirely. Log consumers see a stable schema in modes that produce
  audit trails, and a clean absence in modes that don't.

### 5.2 Plumbing path

```
HTTP request
   ↓
sdkauth.RequireBearerToken middleware
   ↓ (validates JWT, populates ctx + req.Extra.TokenInfo)
mcp.Server tool dispatch
   ↓
register.go shim:  identity := NewIdentityFromTokenInfo(req.Extra.TokenInfo)
   ↓
ToolManager.CallTool(ctx, name, params, identity)
   ↓
logToolResult(... identity ...)   // defer
```

### 5.3 Changes to `createOIDCTokenVerifier`

The claims struct (`middleware.go:125-129`) gains a `Jti` field. The
verifier stashes `client_id` and `jti` into `TokenInfo.Extra` under
fixed string keys (`"client_id"`, `"jti"`). The `Identity` constructor
reads from those keys.

### 5.4 Changes to `CallTool` signature

```go
// before
func (m *ToolManager) CallTool(ctx context.Context, name string, params map[string]any) (*mcp.CallToolResult, error)

// after
func (m *ToolManager) CallTool(ctx context.Context, name string, params map[string]any, id Identity) (*mcp.CallToolResult, error)
```

Test fixtures pass `Identity{}` (the zero value, with `present:false`)
where identity is not under test. This matches disabled-mode behavior —
no identity attributes are emitted — which keeps existing
tool-handler tests unchanged in their log output.

### 5.5 Nil-handling contract at the shim

The shim at `register.go:55` constructs `Identity` from `req.Extra.TokenInfo`,
but BOTH `req.Extra` (the `*RequestExtra` pointer) and `req.Extra.TokenInfo`
(the inner pointer) can be nil:

| Situation | `req.Extra` | `req.Extra.TokenInfo` | Constructor input | `Identity.present` |
|---|---|---|---|---|
| OAuth mode, normal request | non-nil | non-nil | `*TokenInfo` | `true` |
| Static mode, normal request | non-nil | non-nil | `*TokenInfo` | `true` |
| Disabled mode | nil OR non-nil | nil | `nil` | `false` |
| Test constructing bare `CallToolRequest{}` | nil | n/a | `nil` | `false` |

Constructor contract:

```go
// NewIdentityFromTokenInfo builds an Identity for the audit-log layer.
// A nil TokenInfo (disabled mode or test scaffolding) returns an
// Identity with present=false; LogValue then emits no attributes.
func NewIdentityFromTokenInfo(t *sdkauth.TokenInfo) Identity {
    if t == nil {
        return Identity{present: false}
    }
    return Identity{
        present:  true,
        sub:      normalizeAbsent(sanitizeClaim(t.UserID)),
        iss:      normalizeAbsent(sanitizeClaim(extraString(t, "iss"))),
        clientID: normalizeAbsent(sanitizeClaim(extraString(t, "client_id"))),
        jti:      normalizeAbsent(sanitizeClaim(extraString(t, "jti"))),
    }
}
```

`normalizeAbsent(s)` returns `"<absent>"` for empty input, `s`
otherwise. One helper, one sentinel, no side effects. The
constructor records what identity was present; it does not raise
alarms. Alarming on empty identity is telemetry's job (SOL-149791)
and validation's job (a future verifier-tightening ticket; see §8.1).

The shim mirrors this:

```go
var info *sdkauth.TokenInfo
if req.Extra != nil {
    info = req.Extra.TokenInfo
}
identity := NewIdentityFromTokenInfo(info)
return mgr.CallTool(ctx, reg.name, params, identity)
```

## 6. Log line contract (exact wording)

These exact strings are part of the audit-log contract. Operators will
build SIEM rules and dashboards against them; changing them after
release is a breaking change for downstream consumers. Listed here so
they are reviewable in the plan, not discovered in the diff.

### 6.1 Tool-invocation success (existing line, with new fields)

- Level: `INFO`
- Message: `"tool invoked"` (unchanged from today)
- Existing attributes: `tool`, `broker`, `status="success"`, `duration`
- **New attributes (always present in `oauth` / `static` modes):**
  `sub`, `iss`, `client_id`, `jti`

Example (oauth mode, all claims present):
```json
{"level":"INFO","msg":"tool invoked","tool":"get-broker-health","broker":"prod-east","status":"success","duration":"23.4ms","sub":"auth0|abc123","iss":"https://example.auth0.com/","client_id":"cursor-ide","jti":"a3f9..."}
```

Example (oauth mode, IdP didn't issue `client_id` or `jti`):
```json
{"level":"INFO","msg":"tool invoked","tool":"get-broker-health","broker":"prod-east","status":"success","duration":"23.4ms","sub":"auth0|abc123","iss":"https://example.auth0.com/","client_id":"<absent>","jti":"<absent>"}
```

Example (static mode):
```json
{"level":"INFO","msg":"tool invoked","tool":"get-broker-health","broker":"prod-east","status":"success","duration":"23.4ms","sub":"dev-user","iss":"<absent>","client_id":"<absent>","jti":"<absent>"}
```

Example (disabled mode — identity fields entirely absent):
```json
{"level":"INFO","msg":"tool invoked","tool":"get-broker-health","broker":"prod-east","status":"success","duration":"23.4ms"}
```

### 6.2 Tool-invocation error (existing line, with new fields)

- Level: `ERROR`
- Message: `"tool invoked"` (unchanged)
- Existing attributes: `tool`, `broker`, `status="error"`, `error_type`, `duration`, optionally `kind`/`http_status`/`reason_code`/`operation`
- **New attributes:** same identity fields as §6.1

### 6.3 Destructive operation WARN (existing line, with new fields)

- Level: `WARN`
- Message: `"executing destructive operation"` (unchanged)
- Existing attributes: `tool`, `broker`
- **New attributes:** same identity fields as §6.1

### 6.4 Sentinel string value

| Sentinel | Meaning | Where it appears |
|---|---|---|
| `<absent>` | The claim was not present or was empty when we constructed `Identity`. | Any of `sub`, `iss`, `client_id`, `jti` in `oauth` / `static` modes. |

There is exactly one sentinel string. Log consumers grep for one
pattern. The forensic distinction between "auth ran but `sub` was
empty" (alarming) and "IdP did not issue this claim" (normal) lives
elsewhere — operator telemetry tooling (SOL-149791) and a future
verifier-tightening ticket (§8.1) own the "this shouldn't be empty"
concern. This layer records what was there; it does not raise alarms.

The string (including the angle brackets) is part of the contract.
It is deliberately chosen to be visually distinct from real claim
values — no real `sub`/`iss`/`client_id`/`jti` will contain `<` or
`>`.

## 7. Implementation in commits

| # | Commit | Touches |
|---|---|---|
| 1 | Add `client_id` and `jti` to claims; stash in `TokenInfo.Extra` | `internal/auth/middleware.go`, `middleware_test.go` |
| 2 | Introduce `Identity` type + constructor + sanitizer + `LogValue` + tests | `internal/tools/identity.go` (new), `identity_test.go` (new) |
| 3 | Extend `CallTool` signature; thread `Identity` through `logToolResult` and the destructive WARN | `internal/tools/manager.go`, `manager_test.go`, `register.go` |
| 4 | Documentation: README + startup banner note that dev/disabled modes are not an audit trail | `README.md`, `internal/auth/banner.go` (or wherever the banner lives) |

Each commit compiles and passes existing tests on its own. The full PR
is the sum.

## 8. Rejected alternatives

### 8.1 Tighten the verifier to reject tokens missing `sub`/`client_id`

**Considered:** RFC 9068 §2.2 makes `sub` and `client_id` REQUIRED on
OAuth 2.0 JWT access tokens. A strict verifier would 401 such tokens,
eliminating the `<absent>` branch entirely.

**Rejected:** out of scope. This ticket is about *wiring identity into
logs*, not tightening the auth contract. Strict-reject is a
backwards-incompatible policy decision for operators whose IdP
issues non-RFC-9068 tokens (some use `azp` instead of `client_id`,
some omit `client_id` from JWTs and only return it on introspection).

A spec citation note: earlier drafts attributed the phrase "audit log
poisoning" to RFC 9068 §5. That section discusses JWT confusion,
sub manipulation, and key compromise — it does **not** use the phrase
"audit log poisoning" and does not discuss placeholder-claim handling.
The strict-reject argument stands on RFC 9068 §2.2 (claims REQUIRED)
alone; the rhetorical framing has been removed.

### 8.2 Pass identity via context, not as a parameter

**Considered:** `CallTool(ctx, name, params)` unchanged; manager reads
identity from `auth.TokenInfoFromContext(ctx)`. Smaller diff. No test
fixtures need updating.

**Rejected:** rule-in-the-head. "Callers must remember to pass an
auth-attached context" is a discipline rule, not an enforced contract.
A parameter makes the dependency visible in the signature; the
compiler refuses to forget. The cost is a few test-fixture lines
passing `Identity{}`.

### 8.3 Log `*sdkauth.TokenInfo` directly via a wrapper type

**Considered:** `type loggedTokenInfo sdkauth.TokenInfo` with a
`LogValue()` that picks fields. Avoids defining a new struct.

**Rejected:** the wrapper still has the same fields, including
`Extra map[string]any`. The whole point of owning a type was to
prevent `Extra` from leaking values added in future SDK versions.
A wrapper inherits that risk. A separate struct with explicit fields
is the real type-level guarantee.

### 8.4 Log `email`, `preferred_username`, scopes, expiration

**Considered:** richer log lines, easier human reading ("user
foo@example.com ran delete-queue").

**Rejected:**
- `email`, `preferred_username` are higher-sensitivity PII than `sub`
  under GDPR (Recital 26). `sub` is the standards-blessed audit
  identifier (RFC 7519, RFC 9068). If you can audit with `sub` alone,
  do.
- `scope` and `expiration` have no audit value-add. Audit asks "who,
  what, when, against what" — not "what permissions were on the token
  at the time." Forensics can fetch the token's scopes from the IdP
  via `jti` if needed.

### 8.5 Per-line `auth_mode` field

**Considered:** add `auth_mode={oauth|static|disabled}` to every log
line so a reader can never mistake a dev log for an audit trail.

**Rejected:** noise in the 99% prod-OAuth case. The startup banner
already states the mode loudly. Operators doing forensic work read
the banner once; making every line self-documenting is over-correction
for a problem the banner already solves. See §3.3.

### 8.6 Silent omit vs. `<absent>` sentinel for empty claims

**Considered:** when `client_id`/`iss`/`jti` is empty (IdP didn't
issue it), omit the field from the log line entirely. Shorter lines
for non-RFC-9068 deployments.

**Rejected:** schema instability. Log consumers (SIEM rules, grep,
parsers) prefer a stable shape. "Field sometimes present, sometimes
absent" creates ambiguity: is the field missing because the IdP
didn't issue the claim, or because of a code bug? The `<absent>`
sentinel keeps the schema fixed across all tokens within a mode.

See §2 for the full matrix.

### 8.7 Two sentinels (`<missing>` for empty `sub`, `<absent>` for everything else)

**Considered:** distinguish the two cases at the field-value level —
`sub=<missing>` for "auth ran, value was empty" (alarm), `<absent>`
for "IdP didn't issue this claim" (normal). One glance at a log line
tells you which case you're in.

**Rejected:** alarming on a malformed token is not this layer's
concern — it belongs to telemetry (SOL-149791) and a future
verifier-tightening change (§8.1). Distinguishing alarm from data in
the same field also mixes two concerns. Log consumers writing SIEM
rules would memorize two strings; operators reading lines by hand
would have to remember which one means what. A single `<absent>`
sentinel keeps field values as *data*. The alarm channel lives
elsewhere.

## 9. Tests

### 9.1 Behavioral tests

| Test | What it pins |
|---|---|
| `TestIdentity_LogValue_emitsExactlyKnownFields_whenPresent` | `LogValue` with `present:true` emits exactly `sub`/`iss`/`client_id`/`jti` and nothing else. Guards against future-field leaks. |
| `TestIdentity_LogValue_emitsNothing_whenNotPresent` | `LogValue` with `present:false` (disabled-mode) emits an empty group; log line shows no identity attributes. |
| `TestIdentity_LogValue_alwaysEmitsAllFour_inPresentMode` | All four fields emitted even when individual values are `<absent>` / `<absent>`. Stable schema for SIEM. |
| `TestNewIdentityFromTokenInfo_nilTokenInfo` | Nil input → `Identity{present:false}`. No panic. |
| `TestNewIdentityFromTokenInfo_emptyClaims_produceAbsentSentinel` | Empty `UserID` / `iss` / `client_id` / `jti` all normalize to `"<absent>"`. No side effects. |
| `TestSanitizeClaim_stripsControlChars` | CR/LF/ANSI/other control chars stripped; length capped at 256. |
| `TestSanitizeClaim_logInjection_endToEnd` | A `sub` containing `\n[FAKE LOG LINE]` does not produce two log lines when serialized through `slog.NewJSONHandler` AND through `slog.NewTextHandler`. |
| `TestCallTool_logsIdentityFields_success` | A `CallTool` with a built `Identity` produces a JSON log line containing all four identity fields. |
| `TestCallTool_logsIdentityFields_error` | Same on the error path. |
| `TestCallTool_logsIdentity_destructiveWarn` | Destructive-op WARN includes identity fields. |
| `TestCallTool_disabledMode_emitsNoIdentityFields` | `CallTool` with `Identity{present:false}` produces a log line identical in shape to today's (no `sub`/`iss`/`client_id`/`jti` keys). |

Tests capture log output via `slog.New(slog.NewJSONHandler(buf, ...))`
and assert against the JSON, not against any package-global state.

### 9.2 Drift-detection tests (SDK and verifier coupling)

The `Identity` struct depends on contracts we do not own:
`sdkauth.TokenInfo` is defined by the MCP go-sdk, and the OIDC claims
we extract are defined by an external IdP and the `coreos/go-oidc`
library. If any of those silently change shape — a renamed field, a
new `Extra` key, a removed claim — our audit logging could degrade
without warning.

These tests are deliberately tied to *our assumptions about external
contracts*, not to our own logic. When the upstream changes, they
should be the first thing that breaks.

| Test | What it pins |
|---|---|
| `TestTokenInfoStruct_hasExpectedFields` | Reflect over `sdkauth.TokenInfo` and assert the field set is exactly `{Scopes, Expiration, UserID, Extra}`. Adding a new field is fine, but it triggers an explicit test-update event, forcing the human writing the upgrade PR to consciously decide whether the new field belongs in `Identity`. |
| `TestTokenInfoExtra_isMapStringAny` | Assert `reflect.TypeOf(TokenInfo{}.Extra) == reflect.TypeOf(map[string]any{})`. If the SDK ever changes `Extra` to a strongly-typed struct, our `extraString(t, "client_id")` helper breaks and we want to know at test time, not at runtime. |
| `TestVerifier_populatesExpectedExtraKeys` | Construct a fake JWT, run it through `createOIDCTokenVerifier`'s claims extraction, and assert `TokenInfo.Extra` contains the keys `"iss"`, `"client_id"`, `"jti"`. If a future verifier refactor stops populating one of these, this test catches it before audit logs go quiet. |
| `TestClaimsStruct_hasAllAuditedClaims` | Reflect over the anonymous `claims` struct in `createOIDCTokenVerifier` and assert it has the four JSON tags we audit (`sub`, `iss`, `client_id`, `jti`). If someone removes one in a refactor, this catches it. |
| `TestIdentity_audited_fields_matchLogValueOutput` | Reflect over `Identity`'s exported audit surface and assert it matches the keys emitted by `LogValue()`. If a field is added to `Identity` but not wired into `LogValue` (or vice versa), the test fails. This prevents the schema and the emitter from drifting apart silently. |
| ~~`TestSDKVersion_pinned`~~ | **Removed post-implementation — see §13.** The version pin was redundant with `go.mod` review and produced noise on every SDK bump without detecting actual shape changes. The shape-asserting tests above carry the real defense. |

These tests pay for themselves the first time an SDK upgrade or
upstream verifier change introduces a quiet regression. They are
intentionally brittle in one direction (upstream contract change → red
test) and stable in the other (our internal refactors → green).

A comment at the top of the drift-detection test file documents this
intent so a future contributor doesn't "fix" them by loosening the
assertions.

## 10. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `TokenInfo.Extra` is populated in the verifier but `Identity` constructor reads wrong key | Low | Identity fields silently empty | Drift-detection test `TestVerifier_populatesExpectedExtraKeys` pins the `Extra` keys end-to-end. |
| Future SDK upgrade changes `TokenInfo` shape (renamed field, new field, type change) | Low | Audit logs degrade silently or break at compile | Drift-detection tests (§9.2) reflect over `TokenInfo` and `Identity`; any silent shape change fails CI before audit logs go quiet. |
| OIDC verifier refactor stops populating `iss` / `client_id` / `jti` into `Extra` | Low | One or more audit fields silently become `<absent>` for legitimate tokens | `TestClaimsStruct_hasAllAuditedClaims` reflects over the claims struct's JSON tags; removing a tag fails the test. |
| Slog handler changes affect log-injection defense | Low | Theoretical regression | The sanitizer is independent of handler choice; the end-to-end log-injection test runs through both `slog.NewJSONHandler` and `slog.NewTextHandler`. |
| `Identity{}` zero value in test fixtures masks a real missing-identity bug in prod | Medium | False sense of test coverage | Zero-value is `present:false`, matching disabled mode. Integration test for oauth mode specifically asserts a non-empty `sub` field is present. |
| Federated multi-IdP deployment causes `sub` collision in audit log | Low (today: single-IdP only) | Mis-attribution of forensic events | `iss` is logged on every line; SIEM can disambiguate. Validated against RFC 7519 §4.1.2 and OIDC Core §2. |
| Shim test scaffolding constructs bare `CallToolRequest{}` (nil `Extra`) and crashes | Was High; mitigated | Tests cannot exercise the shim | Constructor accepts nil; `Identity.present` field encodes "no identity" cleanly. Test `TestNewIdentityFromTokenInfo_nilTokenInfo` pins behavior. |
| Logs containing `sub` are mishandled by ops (no retention policy) | Out of our control | GDPR exposure | Documented in PR; out of scope for this ticket. |

## 11. References

Direct citations used in this plan (each one validated against the
authoritative source):

- **RFC 7519 §4.1.2** — `sub` claim. *"Use of this claim is OPTIONAL."*
  https://datatracker.ietf.org/doc/html/rfc7519#section-4.1.2
- **RFC 7519 §4.1.7** — `jti` claim. *"The 'jti' value is a case-sensitive
  string."* OPTIONAL.
  https://datatracker.ietf.org/doc/html/rfc7519#section-4.1.7
- **RFC 9068 §2.2** — JWT Profile for OAuth 2.0 Access Tokens; lists
  `iss`, `exp`, `aud`, `sub`, `client_id`, `iat`, `jti` as REQUIRED.
  https://datatracker.ietf.org/doc/html/rfc9068#section-2.2
- **OIDC Core §2** — `sub` is *"locally unique and never reassigned
  identifier within the Issuer for the End-User."*
  https://openid.net/specs/openid-connect-core-1_0.html#IDToken
- **CWE-117** — Improper Output Neutralization for Logs. Describes the
  log-forgery attack the sanitizer mitigates. (CWE describes the
  weakness; explicit "strip CR/LF" prescription is industry practice,
  not a verbatim CWE recommendation.)
  https://cwe.mitre.org/data/definitions/117.html
- **GDPR Recital 26** — Pseudonymous data is still personal data.
  https://gdpr-info.eu/recitals/no-26/
- **NIST SP 800-160** — defense in depth (referenced as principle, not
  for a specific quote).

Adjacent / context:

- **Cloudflare Agents — Authorization.** The one extant MCP-server
  prescription to use `claims.sub` as the canonical user identifier.
  https://developers.cloudflare.com/agents/model-context-protocol/authorization/
- **SOL-149791 FD** — Broker MCP Observability epic (the audit-log
  stream this ticket feeds).
- **`docs/superpowers/specs/2026-05-20-client-auth-mode-design.md`** —
  current auth-mode model (referenced for the dev/disabled-mode
  caveat).

## 12. Revision — flat identity fields, not nested under `"identity"`

**Surfaced during implementation:** the implementation prompt said *"the
field name in the emitted log is `"identity"`"* while plan §6.1's binding
example showed `sub`/`iss`/`client_id`/`jti` as **flat top-level keys**.
These are contradictory. The §6.1 example is what SIEM rules and grep
queries are written against, so it is the binding contract.

**Decision:** emit identity fields **flat** at the top level. Implemented
via `slog.Any("", id)` — Go's `log/slog` package inlines a `GroupValue`
into the enclosing record when the attr's key is the empty string (per
stdlib docs on Group inlining). This:

- Matches plan §6.1's example output verbatim.
- Preserves the disabled-mode property: an empty `GroupValue` from
  `LogValue()` inlines zero attrs, so disabled-mode lines remain
  byte-identical to today's.
- Makes SIEM rules trivial — `sub`, `iss`, `client_id`, `jti` are
  top-level JSON keys, not a nested object that consumers must descend
  into.

This revision does not change Identity's API, fields, or LogValue
implementation — only how the manager wires the value into log calls
(`slog.Any("", id)` instead of `slog.Any("identity", id)`).

## 13. Revision — removed `TestSDKVersion_pinned`

**Surfaced post-implementation:** the implementer landed
`TestSDKVersion_pinned` exactly as §9.2 described — pins a constant
`pinnedSDKVersion = "v1.5.0"` and asserts `go.mod` matches. On review,
the test does not earn its keep.

**What we found.** The test fails on **any** SDK version change,
including safe ones. The signal it produces — "someone modified
go.mod" — is already visible in every PR's diff. It does not detect
the actual risk (a shape change in `TokenInfo` or `Extra`); the
sibling tests `TestTokenInfoStruct_hasExpectedFields` and
`TestTokenInfoExtra_isMapStringAny` do that, and they survive minor
bumps that don't break our assumptions.

**Alternatives considered.**

1. **Keep as-is.** Trains reviewers to mechanically update the
   constant on every SDK bump — the exact "loosen the test to make it
   pass" anti-pattern §9.2's top-of-file comment was meant to prevent.
2. **Weaken to a substring/major-version match.** Still produces
   noise on minor bumps; still doesn't detect shape changes.
3. **Remove the test and constant.** Shape tests carry the real
   defense; go.mod diff carries the deliberate-event signal.

**Decision:** removed `TestSDKVersion_pinned` and `pinnedSDKVersion`.
The `os` import in `identity_test.go` becomes unused and is dropped
with it. The two shape-asserting tests in §9.2 stay; they are the
actual contract enforcement.

**Why this isn't infinite regress.** The remaining drift-detection
tests (`TestTokenInfoStruct_hasExpectedFields`,
`TestTokenInfoExtra_isMapStringAny`,
`TestVerifier_populatesExpectedExtraKeys`,
`TestClaimsStruct_hasAllAuditedClaims`,
`TestIdentity_audited_fields_matchLogValueOutput`) catch every
class of silent regression the version pin was supposed to defend
against, with actionable failure messages and without firing on safe
upgrades. The version pin was redundant with go.mod review.

**Why we missed this in initial planning.** The plan's intent
("force a deliberate event on SDK upgrade") was sound; we missed
that "deliberate event" is already enforced by code review of
`go.mod`. A test that duplicates a review checkpoint adds noise
without signal.

## 14. Revision — `extraString` no longer panics; emits slog.Error + `<verifier-bug>` sentinel

**Surfaced by PR #74 review (bczoma, 2026-05-28):** the original §5.5
chose to panic when `TokenInfo.Extra[key]` was present but not a
string, on the reasoning that "a silent fallback would mask a verifier
bug from every audit log." The reviewer pointed out that the panic
itself *inverts the audit guarantee on exactly the request class where
it matters most*: `NewIdentityFromTokenInfo` is called as an argument
to `mgr.CallTool(...)` in the shim, so a panic during argument
evaluation kills the request before `CallTool`'s `defer logToolResult`
is registered. The result: zero audit log entries for the offending
request — the loud failure the panic was meant to surface produces
*silence* on the audit channel.

**What the review identified.** Both panic and silent fallback fail
the same goal (preserve audit lines) by different mechanisms — panic
deletes the line, silent fallback degrades the line indistinguishably
from "claim was missing legitimately." There is a third option:
**slog.Error + a distinct sentinel.** It keeps loudness (ERROR is
alarmable, the panic stack trace was not more alarmable than that)
while preserving the audit line with a value that ops can distinguish
from the normal "<absent>" case.

**Decision:** `extraString` now emits `slog.Error` naming the offending
key and observed type, then returns a new sentinel
`verifierBugSentinel = "<verifier-bug>"`. The audit line for the
request still emits with the sentinel as the field value. SIEM rules
can grep for `"<verifier-bug>"` to alarm on contract violations,
distinct from `"<absent>"` which signals the normal "IdP did not issue
this claim" case.

**Test changes.** `TestExtraString_panicsOnNonStringValue` becomes
`TestExtraString_nonStringValue_emitsErrorAndReturnsSentinel`, which
asserts (a) no panic, (b) return value is `verifierBugSentinel` and
distinct from `absentSentinel`, (c) an ERROR-level slog entry was
emitted naming the bad key and observed type.

**Why we missed this in initial planning.** The §5.5 reasoning
correctly identified silent fallback as a failure mode; it did not
notice that panic *during argument evaluation in the shim* produces
the same failure mode through a different path. The fix is the
synthesis option the original analysis didn't consider.

## 15. Revision — sanitizer extends to Cf/Zl/Zp categories

**Surfaced by PR #74 review (bczoma, 2026-05-28):** the original
sanitizer (§4.4) stripped only `unicode.Cc` (Control category) plus
an explicit ASCII fast-path for `r < 0x20 || r == 0x7F`. The reviewer
identified CWE-1007 (visual spoofing) as an active attack path: a
malicious IdP issues a sub like `"alice‮nimda"` containing
U+202E RIGHT-TO-LEFT OVERRIDE. The bytes pass through our sanitizer
unchanged because U+202E is category `Cf` (Format), not `Cc`. Any
bidi-aware UI (SIEM dashboards, terminals, JSON viewers) renders the
value visually as `"aliceadmin"`. The audit log attributes the action
to a different user than the one who actually performed it — the
exact attribution guarantee SOL-149606 is meant to provide.

**Decision:** widen the sanitizer's filter from `unicode.IsControl(r)`
to `unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)`.
The four categories are:

- `Cc` — Control characters (CWE-117 surface; covers ASCII C0+DEL+C1).
- `Cf` — Format characters (CWE-1007 surface; bidi overrides, zero-width
  joiners, BOM, soft hyphen, language tags).
- `Zl` — Line separator U+2028 (CWE-117-adjacent in renderers that
  honor it as a line break).
- `Zp` — Paragraph separator U+2029 (same rationale as Zl).

The new filter is a strict superset of the old one; no rune that was
previously stripped is now passed through. Verified by a Go test
iterating `[0x00, 0x1F]` and `{0x7F}` against `unicode.Is(unicode.Cc, r)`
— every codepoint the old explicit ASCII check caught is also in
`Cc`, so the explicit fast-path was redundant and is removed.

**Test added.** `TestSanitizeClaim_stripsBidiAndFormatChars` covers
ten subtests: RLO (U+202E), PDF (U+202C), LRO (U+202D), zero-width
joiner (U+200D), zero-width non-joiner (U+200C), BOM (U+FEFF), soft
hyphen (U+00AD), line separator (U+2028), paragraph separator (U+2029),
and C1 control NEL (U+0085). Inputs use `\u` escapes so the source
file stays pure ASCII — a literal BOM would break Go's parser, and
literal bidi controls in source would make the file dangerous to view
in any bidi-aware editor (the whole point of stripping them).

**Why we missed this in initial planning.** §4.4 framed sanitization
around CWE-117 (log injection via CR/LF) and didn't enumerate CWE-1007
(visual spoofing). The two CWEs share an attacker (IdP-influenced claim
content) and a defender (the sanitizer), but the attack vectors and
the Unicode categories that carry them differ. The original plan named
the attack class implicitly ("strip CR/LF/ANSI/length cap") without
generalizing to "strip non-graphic Unicode that affects rendering."
