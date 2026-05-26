# SOL-149789 — Define and enforce a contract for broker aliases

**Ticket:** https://sol-jira.atlassian.net/browse/SOL-149789
**Branch:** `amorade/SOL-149789`
**Status:** Plan — awaiting implementation

---

## 1. Problem

Broker aliases are the user-defined keys under `brokers:` in the YAML config:

```yaml
brokers:
  prod-east:        # ← this key is the "alias"
    url: https://...
```

They are effectively **public identifiers**: they appear in tool inputs (`broker="prod-east"`), log lines, `list-brokers` output, and error messages. But today there is **no defined contract** for what a valid alias looks like:

- Empty strings accepted (`"": {...}`)
- Whitespace-only / trailing-whitespace aliases load successfully (impossible to spot in YAML)
- Mixed-case typos (`prod` vs `Prod`) silently create two distinct brokers
- No length or charset rules

`yaml.v3` already rejects exact-duplicate keys at unmarshal time. This ticket fills the remaining gaps.

## 2. Contract (from the ticket)

Aligned with RFC 1123 hostname label rules:

| Question | Rule |
|---|---|
| Length | 1–63 (matches RFC 1123 label cap) |
| Chars | `[A-Za-z0-9-]`, must start AND end alphanumeric |
| Case | accepted as written in YAML; preserved in all user-facing output; lowercased only as the internal lookup key |
| Whitespace | reject (no silent trimming) |
| Empty | reject |

**Regex (applied to original casing):**
```
^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$
```

**Case handling:**
- **Internal lookup key:** lowercased form (`prodeast`)
- **User-facing display:** original casing as written in YAML (`ProdEast`)
- Tool calls with `broker="PRODEAST"`, `broker="prodeast"`, or `broker="ProdEast"` all resolve to the same broker
- Logs and `list-brokers` show what the operator actually wrote

**Case-only collisions** (e.g. `Prod` + `prod` in the same config) are rejected loudly at startup with both originals listed.

## 3. Design principles

These came out of the planning conversation and are load-bearing for the implementation choices below.

### 3.1 Error messages contain the rule inline

Validation errors must be **self-sufficient**: the operator should be able to fix the problem from the terminal without opening the README. Pointing to docs forces a context switch and assumes the README will still exist / still be current at the version they're running.

Concrete shapes:

```
broker alias "prod east" is invalid: must be 1-63 characters, contain only letters, digits, and hyphens, and start and end with a letter or digit
```

```
broker aliases "Prod" and "prod" collide: aliases are compared case-insensitively, please rename one
```

The README still documents the contract for people designing configs from scratch — that's a different audience and a different moment.

### 3.2 Case-only collisions are rejected loudly

No silent winner-picking, no "first one wins." If `Prod` and `prod` both appear in the same config, both originals are quoted in a single explicit error. The user must rename one before the server will start.

### 3.3 The type system owns the canonicalization rule (to the extent Go allows)

This is the core architectural principle for this change.

The ticket introduces a new invariant: *"broker lookups are case-insensitive; canonical form is lowercase; user-facing form is the original casing."* That invariant needs an **owner**.

- **If the owner is "every caller"** → every lookup site must remember to lowercase. Forget once → silent bug (returns "broker not found" when the broker exists, just with different case). The rule lives in tribal knowledge.
- **If the owner is the API** → lowercase happens inside the lookup methods. Callers pass any case. The rule lives in the type system. Cannot be violated by accident.

We choose the second. Concretely:

- `cfg.brokers` is **unexported**. No direct map access from outside the config package.
- `cfg.Broker(alias)` and `cfg.BrokerAliases()` are the only ways to look up / enumerate brokers.
- `BrokerPool.getOrCreate(alias)` lowercases at entry. Pool's `GetSEMPv1` / `GetSEMPv2` are case-insensitive by construction.
- `BrokerPool` adds internal `configFor` / `clientFor` / `setClient` helpers — within the pool, **all** map access flows through these, so a future method (e.g. `HealthCheck(alias)`) can't reach the unsafe path by accident.
- `BrokerConfig.displayName` is unexported with a `DisplayName()` accessor — the field is derived from validation, never from YAML, and cannot be mutated by callers after construction.

### 3.3a Honest scope of "type system enforcement" in Go

Go's encapsulation boundary is the **package**, not the type. Lowercase identifiers are visible to every method, function, and file within the same package. There is no `private` keyword that hides a field from other methods on the same struct.

What this means for the invariants above:

| Surface | Enforcement strength |
|---|---|
| External packages accessing `cfg.Brokers` | **Compile-time blocked** (unexported field). Strong. |
| External packages accessing `pool.configs` / `pool.clients` | **Compile-time blocked** (already unexported). Strong. |
| Within `internal/config` accessing `cfg.brokers` directly | Convention + doc comment + obvious accessor. Same-package — Go can't block this. |
| Within `internal/semp` accessing `pool.configs` directly from a future pool method | Convention + helper (`configFor`) + doc comment. Same-package — Go can't block this. |

Achieving compile-time enforcement *within* a package requires splitting the data into a sub-package (e.g. `internal/semp/internal/brokerstore/`). That is over-engineered for two maps and a mutex, so we don't do it.

The realistic enforcement story:

- **Cross-package**: real compile-time enforcement.
- **Within-package**: "the right path is the obvious path; the wrong path is reviewable." Doc comments on the unsafe field, helpers named for the operation, and convention enforced by code review.

This is honest about what we get and what we don't.

### 3.3b This matches standard Go practice

The pattern (canonicalize at the boundary, store canonical form, expose accessor methods, unexport the field across package boundaries, accept within-package convention as the ceiling) is standard for normalized-key maps in idiomatic Go. Reference points in the standard library:

- **`net/http.Header`** — case-insensitive header names. Canonicalizes via `textproto.CanonicalMIMEHeaderKey` inside `Get`/`Set`/`Add`/`Del`. Direct map access compiles but silently misses — the accessor methods are the documented path.
- **`net/url.Values`** — same shape, same pattern.
- **`sync.Map`** — fully hides internals behind methods; chosen when the implementation complexity (lock-free reads) justifies it.

We sit between `http.Header` (everything exported, convention enforced) and `sync.Map` (fully hidden). Specifically, we go one step further than `http.Header` by unexporting the underlying map, because we own both ends of the API surface and don't have backward-compatibility obligations. This is a deliberate "slightly more careful than average" choice — recognizably idiomatic, but with the field unexported because we can.

Two minor places where we exceed typical Go practice:
1. Storing original casing in a separate `displayName` field alongside the canonical map key. A typical Go codebase might just preserve the original case in the map key and lowercase only at lookup; we keep both because we want unambiguous internal access AND user-facing display.
2. Internal pool helpers (`configFor`, etc.) instead of inline `p.configs[strings.ToLower(alias)]` at each site. A typical codebase might skip the helpers; we add them so the convention is loud and so future pool methods inherit the case-folding without thinking about it.

Neither is unusual — both are recognizable as "deliberately careful" rather than "non-idiomatic."

### 3.4 We don't compromise the production API for test ergonomics

Tests adapt to the contract; the contract does not weaken to accommodate tests. If unexporting a field makes test construction inconvenient, the answer is a test-only constructor (package-private or explicitly named) — **not** keeping the field exported "for tests."

### 3.5 Scope discipline

We intentionally do NOT:
- Introduce a new `BrokerAlias` named type (overkill for one map with ~10 entries)
- Introduce a `BrokerSet` domain type (creep beyond ticket scope)
- Refactor the pool's internal maps (already package-private and safe)
- Change composite YAML tool plumbing (runtime alias resolution flows through the pool, which is already covered)
- Change how `manager.go` extracts `broker` from JSON-RPC params (the input surface itself is unchanged)

## 4. Surfaces and how each one becomes safe

| Surface | Today | After this change | Ownership |
|---|---|---|---|
| `pool.GetSEMPv1(alias)` / `GetSEMPv2(alias)` | Case-sensitive map lookup | Case-insensitive via `getOrCreate` | Pool API |
| `cfg.Brokers[alias]` (direct map access) | Public, case-sensitive | **Removed** — field unexported | N/A — path no longer exists |
| `cfg.Broker(alias)` (new accessor) | Doesn't exist | Case-insensitive lookup | ServerConfig API |
| `cfg.BrokerAliases()` (new accessor) | Doesn't exist | Returns display names, sorted | ServerConfig API |
| `BrokerConfig.DisplayName` | Doesn't exist | `displayName()` accessor; original casing | BrokerConfig |
| `pool.configs` / `pool.clients` (direct map access from within pool methods) | Open — any pool method can touch the maps directly | `configFor` / `clientFor` / `setClient` helpers handle case-folding; doc comment marks direct access discouraged | Pool internals — convention within package (see §3.3a) |
| Logs: lazy-creation in `pool.go` | Logs raw caller string | Logs `cfg.DisplayName()` | Plan |
| Logs: tool call sites in `manager.go` | Logs raw caller string | Logs `DisplayName()` after resolution | Plan |
| Validation error messages | N/A | Use original casing (`displayName`) | Plan |
| `list-brokers` output / `pool.Aliases()` callers | Returns raw map keys (today: original casing; after canonicalization without the accessor fix, would silently return lowercase) | Returns display names (sorted) | Plan — `Aliases()` updated in §5.2 to iterate `cfg.DisplayName()` |

The asymmetry between "pool side fully type-system-enforced" and "ServerConfig side optionally enforced" — discussed in the planning conversation — is resolved by unexporting the field. Both surfaces now genuinely own their case-folding, with no caller-side responsibility remaining.

## 5. Files & changes

### 5.1 `internal/config/config.go`

- Add package-level regex:
  ```go
  var brokerAliasPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
  ```
  (Cannot be a Go `const` — `*regexp.Regexp` isn't const-able. Matches the existing `envVarPattern` precedent.)

- `ServerConfig`:
  - Rename `Brokers map[string]*BrokerConfig` → unexported `brokers map[string]*BrokerConfig`
  - Add `Broker(alias string) (*BrokerConfig, bool)` — lowercases `alias`, looks up in `c.brokers`
  - Add `BrokerAliases() []string` — returns `displayName` values from all configured brokers, sorted

- `BrokerConfig`:
  - Add `displayName string` field, `yaml:"-"` to make it explicit it's not from YAML
  - Add `DisplayName() string` accessor
  - Set during `validate()` iteration, before the regex check runs on a given alias (so error messages can use it even for rejected aliases)

- `validate()`:
  - **Phase 1 — per-alias structural validation:**
    - Iterate `cfg.brokers` (original keys)
    - For each alias, set `broker.displayName = alias`
    - If alias doesn't match `brokerAliasPattern`, append error: `broker alias %q is invalid: must be 1-63 characters, contain only letters, digits, and hyphens, and start and end with a letter or digit`
  - **Phase 2 — case-collision detection:**
    - Build `seen := map[string][]string{}` (lowercase form → list of original spellings)
    - For each alias, append to `seen[strings.ToLower(alias)]`
    - For any entry with `len(seen[lower]) > 1`, append error: `broker aliases %q and %q collide: aliases are compared case-insensitively, please rename one` (list all originals if 3+)
  - **Phase 3 — canonicalize the map:**
    - Only if phases 1 + 2 had no errors (or unconditionally, since validation errors abort startup anyway — TBD during implementation)
    - Build a new `map[string]*BrokerConfig` keyed by `strings.ToLower(alias)`
    - Replace `cfg.brokers` with the canonical map
  - **Phase 4 — existing per-broker validation:**
    - The existing loop (URL, auth credentials, etc.) continues, but errors reference `broker.displayName` instead of the map key

- `LoadConfig`:
  - Continues to populate `cfg.brokers` from `yamlConfig.Brokers` — the YAML unmarshal path is unchanged
  - `validate(cfg)` does the canonicalization, so on successful return `cfg.brokers` is lowercase-keyed

### 5.2 `internal/semp/pool.go`

- Add internal helpers (unexported, single-package convention — see §3.3a for honest scope):
  ```go
  // configFor returns the BrokerConfig for alias (any case), or false if unknown.
  // All map access on p.configs MUST go through this helper.
  func (p *BrokerPool) configFor(alias string) (*config.BrokerConfig, bool)

  // clientFor returns the cached BrokerClient for alias (any case), or false.
  // All reads on p.clients MUST go through this helper.
  func (p *BrokerPool) clientFor(alias string) (*BrokerClient, bool)

  // setClient stores a newly-created BrokerClient under the canonical key.
  // All writes to p.clients MUST go through this helper.
  func (p *BrokerPool) setClient(alias string, c *BrokerClient)
  ```
  Each helper does the `strings.ToLower(alias)` itself. Doc comments on `configs` and `clients` fields say *"keyed by canonical (lowercase) alias — use configFor/clientFor/setClient, do not access directly."*

- `getOrCreate(alias string)`:
  - Uses `p.configFor` and `p.clientFor` / `p.setClient` instead of touching `p.configs` / `p.clients` directly
  - No need to lowercase at entry separately — the helpers do it

- `NewBrokerPool`: continues to seed `p.configs` from `cfg.brokers` (which is already canonical-keyed after `validate()`). Decision deferred to implementation: pass the map directly, or build it via `cfg.BrokerAliases()` + `cfg.Broker(alias)` lookup. Either keeps the canonical-key invariant.

- Lazy-creation log line (`pool.go:81`): use `cfg.DisplayName()` instead of the param string
- `Aliases()`: returns `cfg.DisplayName()` values, sorted

**Enforcement note:** Within `internal/semp`, the helpers + doc comment are the strongest signal Go allows. A future pool method that needs to look up a broker has both options visible (`p.configs[alias]` and `p.configFor(alias)`), but the helper is named for the operation and the field carries a "don't touch directly" comment. Compile-time enforcement within the same package would require splitting into a sub-package — out of scope (§8).

### 5.3 `internal/tools/manager.go`

- After successful broker resolution, look up the `BrokerConfig` to get `DisplayName()`
- Use that in the three `slog.String("broker", ...)` log sites: lines 159, 221, 234
- The raw `broker` param string from JSON-RPC is no longer logged — only the canonical display form

**Rationale (from the conversation):**
The tool call param surface is intentionally forgiving (LLMs may call with any case). The log surface should be strict (same broker → same identifier in observability). Normalizing to `DisplayName()` makes these consistent.

### 5.4 `broker-config.example.yaml`

Add a one-line comment above the broker key (around line 56):

```yaml
# Broker aliases: 1-63 chars, [A-Za-z0-9-], must start and end alphanumeric.
# Aliases are case-insensitive — "Prod" and "prod" collide.
brokers:
  my-broker:
    ...
```

### 5.5 `README.md`

Add a short paragraph in the broker config section documenting the alias contract. Keep it brief — error messages are the authoritative source; the README is for people designing a config.

### 5.6 Tests

**Framing.** The alias contract has three distinct concerns, and each deserves its own kind of test:

1. **The contract logic itself** (regex, collision detection, canonicalization) is *pure* — it transforms input maps/strings into output maps/strings with no I/O. Pure logic deserves small, fast, focused tests that don't go through `LoadConfig`, don't need temp files, and don't construct a full `ServerConfig`. Each test reads as a small equation: `f(input) == expected`.
2. **The accessor contract** (`cfg.Broker(alias)` resolves case-insensitively; `cfg.BrokerAliases()` returns display forms) is the user-visible behavior. These tests should exercise the *public API*, not poke at internals, so they remain robust to future internal layout changes.
3. **End-to-end loader behavior** (a real YAML file produces a working config) is an integration test. The existing 19 `cfg.Brokers[...]` reads in `config_test.go` fall into this category — they verify what `LoadConfig` produced, and reading the resulting data directly is fine *because once `LoadConfig` returns, the result is just a value.*

This separation comes from a basic principle: **prefer pure logic with focused tests over impure logic with elaborate test setup.** Small pure helpers are easier to test, easier to reason about, and easier to extend.

#### Layer 1 — Pure-helper tests (new, fast, no fixtures)

The contract logic extracts into **two** pure helpers — not three. The split is deliberate; see the design note below.

```go
// In internal/config/config.go (or a new aliases.go if it grows)
var brokerAliasPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// isValidAlias reports whether s satisfies the broker alias contract.
// Exposed for testing the contract independently of map canonicalization.
func isValidAlias(s string) bool {
    return brokerAliasPattern.MatchString(s)
}

// validateAndCanonicalizeBrokers walks the input map, validates each alias,
// detects case-only collisions, sets displayName on each broker, and returns
// a new map keyed by canonical (lowercase) alias. All errors are accumulated
// and returned together so operators see every issue in one run.
func validateAndCanonicalizeBrokers(brokers map[string]*BrokerConfig)
    (canonical map[string]*BrokerConfig, errs []error)
```

**Why two functions, not three.** A naive split would also extract `detectAliasCollisions(aliases []string) [][]string` as its own function. Skipped because:
- Collision detection is a few lines of map-grouping by `strings.ToLower` — no nontrivial edge cases that aren't visible at the map level.
- A separate function adds a name without adding a real abstraction. Reading `validateAndCanonicalizeBrokers`, the inline grouping is as clear as a function call would be.
- Heuristic: *a function deserves to exist when it has a real, namable concept AND nontrivial edge cases worth testing in isolation.* `isValidAlias` clears both bars (alias contract is a real concept; regex boundary cases benefit from focused testing). Collision detection clears the first weakly and fails the second.

**Why `isValidAlias` is worth its own function.** The regex has many boundary cases (empty, whitespace, length 1, length 63, length 64, leading/trailing hyphen, unicode, embedded special characters). Testing these through `validateAndCanonicalizeBrokers` would require constructing a full `map[string]*BrokerConfig` and asserting against the `errs` slice for every case — heavy ceremony for what is fundamentally a one-input/one-output check. A direct predicate keeps the table-driven test small and readable.

**Tests:**

- `isValidAlias` — table-driven, ~15 cases:
  - Valid: `"prod"`, `"ProdEast"`, `"prod-east-1"`, `"a"`, single char alphanum, 63-char boundary string
  - Invalid: empty `""`, whitespace-only `"  "`, leading hyphen `"-prod"`, trailing hyphen `"prod-"`, embedded space `"prod east"`, underscore `"prod_east"`, dot `"prod.east"`, 64-char overflow, leading/trailing whitespace `" prod"`, unicode (`"prodé"`)

- `validateAndCanonicalizeBrokers` — table-driven, ~6–8 cases:
  - Happy path: `{"prod-us": ..., "dev": ...}` → canonical map with same keys (already lowercase), `displayName` set, no errors
  - Mixed-case input: `{"ProdEast": ...}` → key becomes `"prodeast"`, `displayName == "ProdEast"`
  - Case-only collision: `{"Prod": ..., "prod": ...}` → error message names both originals
  - 3-way collision: `{"Prod": ..., "PROD": ..., "prod": ...}` → error names all three
  - Invalid alias: `{"prod east": ...}` → error with the exact §3.1 message; canonical map omits it (or includes it canonicalized with displayName set — decision per implementation choice on whether validation aborts canonicalization)
  - Mixed valid + invalid: valid entries canonicalized, invalid reported in `errs`
  - Empty input map: returns empty canonical map, no errors

#### Layer 2 — Accessor contract tests (new, through the public API)

In `internal/config/config_test.go`, new test functions that go through `cfg.Broker(alias)` and `cfg.BrokerAliases()`:

- `TestBrokerLookupIsCaseInsensitive`: load a config with `"ProdEast"`. Assert `cfg.Broker("PRODEAST")`, `cfg.Broker("prodeast")`, `cfg.Broker("ProdEast")` all return the same `*BrokerConfig` (pointer equality).
- `TestBrokerDisplayNamePreservation`: load with `"ProdEast"`. Assert `cfg.Broker("prodeast").DisplayName() == "ProdEast"`.
- `TestBrokerAliasesReturnsDisplayForms`: load with `{"ProdEast", "DevWest"}`. Assert `cfg.BrokerAliases() == ["DevWest", "ProdEast"]` (sorted, original casing preserved).
- `TestBrokerLookupUnknownReturnsFalse`: `cfg.Broker("nonexistent")` returns `(nil, false)`.

The point of these tests is the **behavior visible to users** — they will survive any future refactor of the underlying storage as long as the API contract holds.

#### Layer 3 — End-to-end loader tests (existing + small additions)

The existing 19 `cfg.Brokers[...]` reads in `config_test.go` stay as integration tests. They go through `LoadConfig` (real path: file → parse → validate → return), then verify the produced config. Same-package access means the field rename `cfg.Brokers` → `cfg.brokers` is mechanical; the tests don't otherwise change.

New end-to-end tests to add:
- `TestLoadConfigRejectsInvalidAlias`: YAML with `"prod east": ...` → `LoadConfig` returns an error containing the exact message from §3.1
- `TestLoadConfigRejectsCaseCollision`: YAML with both `"Prod": ...` and `"prod": ...` → error names both originals
- `TestLoadConfigPreservesDisplayName`: YAML with `"ProdEast": ...` → after load, `cfg.Broker("prodeast").DisplayName() == "ProdEast"`

**Style guidance for new tests:** prefer `cfg.Broker(alias)` over `cfg.brokers[alias]`. The existing direct-access reads are grandfathered (mechanical rename only), but new tests should exercise the public API so they remain stable if internals shift.

#### `internal/semp/pool_test.go`

- Existing helper `newTestServerConfig` converts from struct literal to either a `LoadConfig`-via-YAML-fixture call OR a new `config.NewServerConfigForTest` helper (decision in §6 below)
- New test: mixed-case `pool.GetSEMPv1("PROD-US")` returns the same client as `pool.GetSEMPv1("prod-us")` (pointer equality)
- New test: `pool.Aliases()` returns original casing from the source config (sorted display forms)

#### `internal/tools/manager_test.go`, `internal/tools/register_test.go`

- `newTestPool` / `newRegTestPool` helpers convert similarly to §6
- No new test assertions needed beyond confirming existing tests still pass after the field rename and helper conversion

#### `internal/auth/middleware_test.go`, `banner_test.go`

- **Zero changes.** These tests build `ServerConfig` literals but never populate `Brokers` — the field stays at its zero value, which is unaffected by unexporting.

#### Coverage summary

| Layer | New tests | Existing tests touched |
|---|---|---|
| 1: pure helpers (`isValidAlias` + `validateAndCanonicalizeBrokers`) | 2 test functions, ~21 table-driven cases total | none |
| 2: accessor contract | ~4 | none |
| 3: end-to-end loader | ~3 new + 19 renamed | 19 (mechanical `cfg.Brokers` → `cfg.brokers`) |
| Pool case-insensitive | ~2 | 3 helper conversions |
| Auth package | 0 | 0 |

**Net new tests: ~11 test functions (~30 cases total).** Most are small table-driven cases against pure functions, so total LOC and runtime impact is small.

## 6. Test construction strategy

Decision needed during implementation, but the shape is clear:

The current pattern in 3 test helpers (`pool_test.go`, `manager_test.go`, `register_test.go`):

```go
cfg := &config.ServerConfig{
    Brokers: map[string]*config.BrokerConfig{
        "dev":  {URL: ..., Auth: ...},
        "prod": {URL: ..., Auth: ...},
    },
    SEMP: testSEMPCfg(),
}
```

After unexporting, this no longer compiles. Two options:

**Option A — package-private test helper in `internal/config/`:**

Add `newServerConfigForTest(brokers map[string]*BrokerConfig, sempCfg SEMPConfig) *ServerConfig` in `internal/config/config_test.go` (or a `testutil_test.go` file). Other packages can't import test-only code from another package, so this only helps config's own tests.

**Option B — exported constructor with a `_test` suffix or build tag:**

Add `config.NewForTesting(...)` documented as test-only. Less elegant but accessible from other packages' tests.

**Option C — YAML fixture + `LoadConfig`:**

Each helper writes a small YAML to a temp file and calls `LoadConfig`. More boilerplate, but exercises the real production code path — which is arguably a feature (the test confirms `LoadConfig` works end-to-end).

**Recommendation:** Option C for the 3 cross-package test helpers (it's only ~5–10 lines of setup using `t.TempDir`), and option A for any config-package-internal tests that need to skip validation. This keeps the production API honest — there is no "skip validation" escape hatch exported to the world.

If Option C proves too painful during implementation, fall back to Option B with a clearly named `config.NewForTesting` constructor. Decide concretely at the keyboard.

## 7. Acceptance criteria (from ticket, mapped to plan)

- [x] **Regex validation added to `validate()` in `internal/config/config.go`** → §5.1, phase 1
- [x] **Empty alias rejected with a clear error** → covered by regex (empty fails `^[A-Za-z0-9]`)
- [x] **Whitespace in alias rejected with a clear error (no silent trimming)** → covered by regex
- [x] **Case-insensitive duplicate detection** → §5.1, phase 2; error lists all originals
- [x] **Original casing preserved on `BrokerConfig` for use in logs, `list-brokers`, errors** → §5.1, `displayName` field
- [x] **Pool lookup uses lowercased form for map access** → §5.2; tool call with any-case `broker` param resolves correctly
- [x] **All checks run in `validate()` at startup** → §5.1; server exits before binding port
- [x] **Tests added for all required cases** → §5.6
- [x] **`broker-config.example.yaml` updated** → §5.4
- [x] **README config section documents the alias contract** → §5.5

## 8. Out of scope

- Not changing `yaml.v3`'s exact-duplicate-key behavior (already errors at unmarshal)
- Not adding new env var / CLI overrides for aliases
- Not touching composite YAML tool definitions (they reference `broker` at runtime via the resolved alias, which the pool already handles)
- Not introducing new domain types (`BrokerAlias`, `BrokerSet`) — scope creep beyond ticket
- Not changing the `broker` param extraction logic in `manager.go` — input surface unchanged

## 9. Sparks deliberately left in place

For honesty, three minor "rule in your head" sparks remain after this change. All are judged acceptable for the current codebase. Two are fundamental Go-language limitations (within-package access); one is a domain-modeling choice.

1. **Within-package access to `cfg.brokers` and `pool.configs`/`pool.clients`.** Go has no method-private fields — within the same package, any function can touch any unexported field. Mitigated by: doc comments on the fields explicitly marking direct access as discouraged, accessor methods named for the operation, and the fact that the realistic threat (a future contributor adding a method that bypasses the accessor) is reviewable in code review. Closing this fully would require splitting `ServerConfig`'s broker map into a sub-package and the pool's maps into a sub-package — over-engineered for the actual blast radius. Documented in §3.3a so future readers don't misread the enforcement story.

2. **`alias` flowing through `manager.go` as a raw `string`.** A `BrokerAlias` named type would distinguish "raw input" / "canonical" / "display" at the type level. Not worth it for one input surface with ~10 brokers. Mitigated by: the only meaningful operation on the raw string is passing it to the pool (which is safe), or looking up via `cfg.Broker()` (also safe). No call site needs to know the internal canonical form.

3. **Future log sites could log a raw alias string instead of `DisplayName()`.** Cannot be enforced by the type system without a named type. Mitigated by: making `DisplayName()` the natural thing to grab — after `cfg.Broker(alias)`, you have a `*BrokerConfig` whose `DisplayName()` is the obvious choice for any subsequent logging.

If the broker model grows (multi-tenant, dynamic registration, multiple input surfaces, or many more brokers per process), revisit and consider introducing a `BrokerAlias` named type and/or splitting broker storage into a sub-package. Not now.

## 10. Implementation order

1. Add `brokerAliasPattern`, `displayName` field, `DisplayName()` accessor on `BrokerConfig`
2. Add `Broker(alias)` and `BrokerAliases()` accessors on `ServerConfig`
3. Update `validate()` with the 4 phases (per-alias regex, collision detection, canonicalization, existing per-broker checks updated to use `displayName`)
4. Unexport `cfg.Brokers` → `cfg.brokers`
5. Update `internal/semp/pool.go` — add `configFor` / `clientFor` / `setClient` internal helpers, route `getOrCreate` (and all map access) through them, add doc comments on `configs` / `clients` fields, route logs through `DisplayName()`
6. Update `internal/tools/manager.go` — route the 3 log sites through `DisplayName()`
7. Decide test construction strategy (§6); update 3 test helpers
8. Add new tests in `config_test.go` and `pool_test.go`
9. Update `broker-config.example.yaml` and `README.md`
10. Run full test suite; run `/check-logs` per project convention before commit

## 11. Complete impact inventory

This section enumerates every call site that needs to change, based on grepping the codebase. Use this as the implementation checklist.

### 11.1 Production code (non-test) — direct map / field access

| File:Line | Today | After |
|---|---|---|
| `internal/config/config.go:269` | `Brokers: raw.Brokers,` (in `LoadConfig`, populating `ServerConfig`) | Stays — internal to config package, field rename only (`brokers: raw.Brokers`) |
| `internal/config/config.go:461` | `if len(cfg.Brokers) == 0` | `if len(cfg.brokers) == 0` (same-package access) |
| `internal/config/config.go:465` | `for _, alias := range slices.Sorted(maps.Keys(cfg.Brokers))` | `cfg.brokers` (same-package); iteration logic updated for the 4-phase validation flow described in §5.1 |
| `internal/config/config.go:466` | `validateBroker(alias, cfg.Brokers[alias], ...)` | Per-broker validation updated to use `broker.displayName` for error message construction |
| `internal/semp/pool.go:41` | `configs: cfg.Brokers` (in `NewBrokerPool`) | `cfg.BrokerAliases()` + `cfg.Broker(alias)` to build canonical-keyed map, OR a new method on `ServerConfig` that returns the canonical map. Decision deferred to implementation (§5.2). |
| `cmd/server/main.go:265` | `slog.Int("broker_count", len(cfg.Brokers))` (startup log) | `slog.Int("broker_count", len(cfg.BrokerAliases()))` OR add a `cfg.BrokerCount() int` accessor. **Cross-package access — will not compile after unexporting unless changed.** |

**Production references summary:** 6 total — 4 within `internal/config` (same-package, becomes lowercase field rename), 1 in `internal/semp` (becomes accessor call), 1 in `cmd/server` (**cross-package break — must use accessor**).

### 11.2 Production code — log sites that need `DisplayName()` routing

| File:Line | Today | After |
|---|---|---|
| `internal/semp/pool.go:81` | `slog.String("broker", alias)` (in lazy-creation log) | `slog.String("broker", cfg.DisplayName())` |
| `internal/tools/manager.go:159` | `slog.String("broker", brokerAlias)` (destructive-tool warning) | Route through `DisplayName()` — requires fetching the resolved `*BrokerConfig` from pool/config after lookup |
| `internal/tools/manager.go:221` | `slog.String("broker", *broker)` (success path in `logToolResult`) | Same — `*broker` should already be the display form by this point; needs verification at the keyboard |
| `internal/tools/manager.go:234` | `slog.String("broker", *broker)` (error path in `logToolResult`) | Same as above |
| `internal/tools/manager.go:283` | `unknown broker %q` (in `classifyBrokerError`) — error message echoes raw alias | Echo the *raw* form here, because that's what the operator/LLM passed and they need to see their own input. Plus include `pool.Aliases()` which now returns display forms. ✅ already correct |
| `cmd/server/main.go:289` | `slog.Any("broker_aliases", pool.Aliases())` (startup banner log) | Automatically gets display form once `pool.Aliases()` is updated per §5.2 — but listed here for completeness. This is the canonical "configured brokers" log at startup. |

**Log site decision: `logToolResult`'s `*broker` pointer.** It's currently set at `manager.go:120` from `params["broker"].(string)` — i.e., the raw caller input. For consistency with the rest of the spec (logs show display form), this pointer should be updated to the display form *after* broker resolution succeeds. Concrete change: after the successful `pool.GetSEMPv1` / `GetSEMPv2` call, fetch the `*BrokerConfig` (via `cfg.Broker(alias)` or a new pool method) and set `brokerAlias = broker.DisplayName()`. This propagates the display form to all three log sites (159, 221, 234) automatically.

**Out of scope for log normalization:** `internal/semp/resilience/sender.go:111,156` and `retry.go:92,97,108,118` log `d.brokerURL` — these are URL-keyed, not alias-keyed. No change.

### 11.3 Test files — direct `cfg.Brokers[...]` reads (broken by unexporting)

These all live within `internal/config` package itself, so they become field rename only (lowercase `b`). **No structural change needed** since they're same-package — they just need a mechanical rename.

| File | Sites | Action |
|---|---|---|
| `internal/config/config_test.go` | Lines 61, 62, 65, 113, 114, 117, 118, 120, 121, 246, 300, 301, 326, 327, 353, 472, 539, 540, 1210 (19 sites) | Mechanical rename `cfg.Brokers` → `cfg.brokers` (same package). Alternatively, prefer the new `cfg.Broker(alias)` accessor in new test code; existing reads can use either form. |

**Note:** The 19 sites in `config_test.go` use the map read form `cfg.Brokers["prod-us"]` to fetch a single broker. Per §3.4 (production API discipline) and to align with how external callers will access brokers, the cleanest replacement is `cfg.Broker("prod-us")` (returning `(*BrokerConfig, bool)`). This is a bigger diff (each read becomes two lines: get-with-ok, dereference), but it exercises the production API in tests. Decision deferred to implementation — at minimum the field rename is required.

### 11.4 Test files — `ServerConfig` struct literals that populate `Brokers`

These need to change because the field is no longer exported and is no longer reachable from outside `internal/config`.

| File:Line | Helper / Test | Action |
|---|---|---|
| `internal/semp/pool_test.go:29` | `newTestServerConfig` | Convert per §6 — Option C (YAML fixture + `LoadConfig`) or Option B (exported test constructor) |
| `internal/tools/manager_test.go:49` | `newTestPool` | Same |
| `internal/tools/register_test.go:26` | `newRegTestPool` | Same |

### 11.5 Test files — `ServerConfig` struct literals that do NOT populate `Brokers` (zero changes)

These build `ServerConfig` only for `ClientAuth`-related testing. The unexported `brokers` field stays at its zero value (`nil` map) — unaffected by the change.

| File | Sites | Action |
|---|---|---|
| `internal/auth/middleware_test.go` | Lines 46, 87, 172, 291, 329, 367, 405, 443, 482, 516, 552, 653, 713, 720, 727 (15 sites) | **No change required** |
| `internal/auth/banner_test.go` | Lines 40, 60, 80 (3 sites) | **No change required** |
| `internal/config/config_test.go:1418` | `cfg := &ServerConfig{ClientAuth: ...}` | **No change required** (same package; could even still write `brokers: ...` if needed) |

**Total auth-package test sites unaffected:** 18.

### 11.6 Test files — `BrokerConfig` literals (unaffected)

`BrokerConfig` struct literals (e.g. `&config.BrokerConfig{URL: "...", Auth: ...}`) are unaffected by this change because:
- The new `displayName` field is unexported, so callers can't set it — they shouldn't, it's derived
- All other fields remain exported with no rename

The following tests construct `BrokerConfig` literals and need **no change** (12 sites total):
- `internal/semp/broker_test.go` lines 22, 52, 94, 128 (4 sites)
- `internal/semp/sempv2/client_test.go` lines 26, 757, 905, 979 (4 sites)
- `internal/semp/sempv1/client_test.go` line 26 (1 site)
- `internal/semp/resilience/transport_test.go` lines 36, 68 (2 sites)
- `internal/config/config_test.go:736` (in-package, fine either way) (1 site)

These construct individual `*BrokerConfig` values for testing client-level behavior — they never go through alias canonicalization because they don't put the config into a map.

### 11.7 Composite engine — no changes

`internal/composite/executor.go` does not access broker aliases — it operates on `sempv2.Client` directly, with the `broker` param already stripped from `Params` (executor.go:80-87). Alias resolution happens entirely upstream in `manager.go` before the composite engine is invoked. No changes needed.

### 11.8 Summary table

| Category | Count | Notes |
|---|---|---|
| Production files modified | 4 | `config.go`, `pool.go`, `manager.go`, `cmd/server/main.go` |
| Test files modified | 4 | `config_test.go` (rename + new tests), `pool_test.go`, `manager_test.go`, `register_test.go` |
| Test files unaffected | ~8 | `middleware_test.go`, `banner_test.go`, `broker_test.go`, `client_test.go` (×2), `transport_test.go`, composite tests, etc. |
| Doc files modified | 2 | `broker-config.example.yaml`, `README.md` |
| New tests added | ~10–15 | per §5.6 — regex valid/invalid, empty, whitespace, collisions, mixed-case lookup, display preservation, `BrokerAliases` ordering |

### 11.9 Risk register

**Build-breaking change to watch:** `cmd/server/main.go:265` — `len(cfg.Brokers)` is a cross-package access. After unexporting, this **will not compile** until replaced with `len(cfg.BrokerAliases())` or a new `cfg.BrokerCount() int` accessor. This is the only `cmd/server` site touching the field, and it's the most important miss to catch *before* any other change lands — without it, `go build ./...` will fail at the first try.

**Highest-risk design choice:** `internal/semp/pool.go:41` — `NewBrokerPool` currently takes `cfg.Brokers` directly. The replacement path depends on whether we expose a canonical-keyed map accessor on `ServerConfig` or have the pool build its internal map by iterating `cfg.BrokerAliases()`. Either works; decide at the keyboard once the accessor shape is concrete. **Updated post-implementation:** see §12 — the actual answer was "neither map approach; pool holds `*ServerConfig` instead."

**Easy-to-miss change:** `internal/config/config_test.go` has 19 `cfg.Brokers[...]` read sites — easy to forget some during a search-and-replace. Use `grep -n "cfg\.Brokers" internal/config/config_test.go` as a final check before compiling.

**Test-helper conversion:** the 3 cross-package test helpers (§11.4) are the only places where the unexporting actually forces a structural change. Their conversion is mechanical but needs the Option-A-vs-C decision from §6 settled first.

## 12. Mid-implementation correction — pool's `configs` field

This section was added **after** initial implementation finished. It documents a design issue discovered during review of the implementation diff, the investigation that grounded the decision, and the resulting follow-up change. Captured here so the plan reflects what was actually built, not just the original intent.

### 12.1 What we found

The original plan (§5.2) assumed `BrokerPool` would continue to hold its own `configs map[string]*config.BrokerConfig` field, populated at construction from `ServerConfig`. The implementation agent built that, and added an exported `ServerConfig.Brokers()` accessor (returning the live internal map) so the pool's constructor could pull it in.

Review of the resulting diff surfaced two related concerns:

1. **`Brokers()` re-opens the encapsulation hole we deliberately closed in planning.** Unexporting `brokers` was justified by §3.3 ("the type system owns the canonicalization rule"). Adding a method that returns the live map by reference makes the unexport cosmetic — any caller can still mutate, iterate case-sensitively, or otherwise bypass canonicalization through the returned reference. Doc-comment-and-discipline does not match the planning principle.

2. **`pool.configs` was never a separate source of truth.** `p.configs = cfg.Brokers()` is a Go map assignment — reference, not copy. The pool and `ServerConfig` aliased the *same* underlying map. This means we had **one source of truth wearing two names**, which is worse than two real copies: a reader sees `pool.configs` and assumes the pool owns it; a writer mutating either name silently reaches into the other.

### 12.2 Independent investigation

To verify whether `pool.configs` had any genuine purpose we'd missed, an independent agent audited the pre-change pool (`c29a9df:internal/semp/pool.go`). Findings:

- **Three readers, all replaceable.** `p.configs` is read in exactly three places: a keyed lookup in `getOrCreate` (equivalent to `cfg.Broker(alias)`), and `len`/`range` in `Aliases()` (equivalent to `cfg.BrokerAliases()`).
- **No mutations, ever.** The pool never writes to `p.configs` after `NewBrokerPool` sets it.
- **No other readers.** `p.configs` is not touched outside `pool.go`.
- **The original assignment was an alias, not a copy.** `p.configs = cfg.Brokers` (pre-change) shares the map with `ServerConfig`.
- **Historical reason for the field's existence:** at the time `pool.go` was written, `ServerConfig.Brokers` was a plain exported map field with no accessor API. The pool projected the public field into a local struct field to narrow its dependency surface ("the pool needs alias→config and SEMP settings, not the whole `ServerConfig`"). That's a defensible choice in a pre-accessor world.

**Investigation's bottom line:** *"Once `ServerConfig` gains alias-lookup and alias-enumeration accessors (which this ticket adds), `p.configs` becomes a redundant aliased reference and the pool can equivalently hold `*config.ServerConfig`."*

### 12.3 Decision

Remove `configs` from `BrokerPool`. Hold `*config.ServerConfig` directly.

**Why this is the right call (not scope creep):**

- The `configs` field is a *vestige of the pre-accessor world this ticket itself eliminates*. Removing it is the natural completion of the type-system enforcement work we already committed to (§3.3, §3.3a), not a separate refactor.
- It eliminates the `Brokers()` accessor that contradicted §3.3.
- It eliminates the one-source-of-truth-with-two-names ambiguity that the original plan inherited without examining.
- The diff is small (~15 lines, mostly deletions) and doesn't touch the pool's external API.
- We are already in this code with full context. Deferring to a follow-up ticket means re-loading context later AND shipping a known-redundant field in the interim.

**What changes:**

- `BrokerPool.configs` field removed.
- `BrokerPool` gains a `cfg *config.ServerConfig` field (single source of truth).
- `NewBrokerPool(cfg *config.ServerConfig)` stores `cfg` directly; no map copying, no construction loop.
- `configFor(alias)` becomes a one-liner: `return p.cfg.Broker(alias)`.
- `Aliases()` becomes `return p.cfg.BrokerAliases()` (already returns display forms; no sort needed at this layer since `BrokerAliases()` sorts).
- `BrokerConfig(alias)` (used by `manager.go` for display-name resolution) becomes `return p.cfg.Broker(alias)`.
- `clientFor` / `setClient` and the `clients` map are **unchanged** — the client cache is genuinely the pool's own state, not a vestige.
- `ServerConfig.Brokers()` method is **removed**.

**What stays the same:**

- Pool's external API: `GetSEMPv1`, `GetSEMPv2`, `Aliases`, `Close`, `BrokerConfig` — all unchanged signatures.
- Tests at the pool's public boundary — no changes needed.
- `manager.go` integration — unchanged.

### 12.4 Updated enforcement story

Previously (§3.3a table), the within-package "pool internal access" row described `pool.configs`/`pool.clients` as needing convention-based discipline. After this correction:

| Surface | Enforcement strength (revised) |
|---|---|
| External packages accessing `cfg.brokers` | Compile-time blocked. Strong. |
| External packages accessing `pool.clients` | Compile-time blocked (already unexported). Strong. |
| Pool-internal access to broker configs | **Routes through `p.cfg.Broker(alias)` — same accessor external callers use.** No second map to discipline. Strong by collapse: there is no longer a within-package alternative path to discipline against. |
| Pool-internal access to `pool.clients` | Convention + helper (`clientFor`/`setClient`) + doc comments. Same-package — Go cannot block this. (Unchanged from §3.3a.) |

The encapsulation story is *cleaner* after this correction than the original plan envisioned: one of the two "convention-enforced" within-package surfaces collapses into "the surface no longer exists." The other (`clients` map) genuinely is pool-owned state and keeps its convention-based protection.

### 12.5 Why we missed this in planning

Honest retrospective for the record: we accepted the pool's existing data shape ("it holds a configs map") and worked within it. That's a reasonable scoping choice — you can't audit every adjacent design decision in one ticket — but it meant we patched around a structure that didn't need to exist. Surfaced only when reviewing the implementation diff and asking "what problem does this `Brokers()` workaround actually solve?"

Generalizable lesson: **when introducing an invariant-enforcing API (here: the accessor methods), audit existing structures that hold the same data via the old API path.** They may have been correct only in the absence of the new API.

## 13. Second mid-implementation correction — narrow the pool's config dependency to an interface

This section captures a second correction discovered during pre-commit review of §12's refactor. Like §12, it is documented here so the plan reflects what was actually shipped, not just the original intent.

### 13.1 What we found

§12 replaced `BrokerPool.configs map[string]*config.BrokerConfig` with `BrokerPool.cfg *config.ServerConfig` — pool now holds the whole config instead of a redundant map. This solves the "one source of truth wearing two names" problem.

But the new shape has its own asymmetry with the planning principle in §3.3 ("the type system owns the canonicalization rule"). A `*config.ServerConfig` field exposes the entire config struct to any pool method:

- Reads: `p.cfg.Port`, `p.cfg.ClientAuth.DevToken`, `p.cfg.LogLevel`, etc. — all compile and execute
- Writes: `p.cfg.Port = 9999`, `p.cfg.ClientAuth.Mode = "..."`, etc. — also compile and execute

The pool only uses two operations on config: `cfg.Broker(alias)` (for `getOrCreate`'s lookup) and `cfg.BrokerAliases()` (for `Aliases()`). Every other field on `ServerConfig` is *available but irrelevant* — which is exactly the failure shape we worked to eliminate for `cfg.brokers` in the original ticket.

### 13.2 The principle, applied consistently

For `cfg.brokers`, the planning argument was:

> *"If both safe and unsafe paths exist, contributors will use the unsafe one because it doesn't look unsafe."* (§3.3a)

That principle applies identically here. `p.cfg.Port = 9999` written by a future contributor adding a pool method looks like normal Go. Nothing about the line screams "wait, that's outside this struct's purpose." The pool's role is broker resolution and client caching — not port configuration, not auth mode, not logging level. But the compiler can't see the boundary because `*ServerConfig` exposes everything.

The fix is the same shape we used for `cfg.brokers`: **make the unsafe surface structurally unreachable.** For the config map, we unexported the field. For the pool's config dependency, we narrow the type to an interface that exposes only what's actually needed.

### 13.3 The interface

```go
// In internal/semp/pool.go:

// BrokerSource is the minimum config surface the pool depends on.
// Implemented implicitly by *config.ServerConfig via Go's structural typing.
type BrokerSource interface {
    Broker(alias string) (*config.BrokerConfig, bool)
    BrokerAliases() []string
}
```

Two methods. Nothing else. `*config.ServerConfig` satisfies it (the methods already exist with these exact signatures, added in commit 1). No code change required on the config side.

### 13.4 What this gives us, concretely

| Surface | Before §13 | After §13 |
|---|---|---|
| `p.cfg.Broker(alias)` | Works | Works (via interface dispatch) |
| `p.cfg.BrokerAliases()` | Works | Works (via interface dispatch) |
| `p.cfg.Port` (read) | Compiles | **Does not compile** |
| `p.cfg.ClientAuth.Mode = "x"` (write) | Compiles | **Does not compile** |
| `p.cfg.SEMP` (read) | Compiles | **Does not compile** |

The interface mechanically prevents the pool from depending on *any* config field beyond broker resolution. The compiler enforces the boundary. No convention, no doc comment, no code review vigilance required.

This is the same enforcement level we get for cross-package access to `cfg.brokers` — compile-time blocking, not convention.

### 13.5 Why this is enforcement-by-construction, not over-engineering

The standard test for "interface worth its keep" is: does it earn at least one of {multiplicity, substitutability, narrowing}?

- **Multiplicity:** Today, one implementation (`*config.ServerConfig`). No second one planned.
- **Substitutability:** No need today.
- **Narrowing:** **Yes.** The pool's interface to config goes from ~10 fields + 2 methods (full struct) to 2 methods. Compile-time enforcement against accidental coupling to the rest.

Narrowing alone is sufficient when the rejected surface is large and the risk of misuse is concrete. Here it's both: `ServerConfig` carries auth tokens, ports, logging config, TLS paths — none of which the pool should ever touch.

The cost is one new type in one file. Read by anyone who reads `pool.go`. Tiny.

### 13.6 What does NOT change

- **Pool's external API:** `NewBrokerPool`, `GetSEMPv1`, `GetSEMPv2`, `Aliases`, `Close`, `BrokerConfig` — all unchanged. `NewBrokerPool` still takes `*config.ServerConfig` (Go's structural typing means the caller doesn't have to know about the interface).
- **`ServerConfig` itself:** No new methods, no removed methods, no renamed methods. `Broker(alias)` and `BrokerAliases()` already existed (commit 1).
- **Tests:** No changes required. Existing tests construct `BrokerPool` via `LoadConfig` + `NewBrokerPool` — same path.

### 13.7 Updated enforcement table (replaces §3.3a / §12.4 table)

| Surface | Enforcement strength |
|---|---|
| External packages accessing `cfg.brokers` | Compile-time blocked. Strong. |
| External packages accessing `pool.clients` | Compile-time blocked. Strong. |
| Pool-internal access to broker configs | **Routes through `p.src.Broker(alias)` / `p.src.BrokerAliases()` — and only those two operations are reachable. Compile-time enforced.** |
| Pool-internal access to other config fields (Port, ClientAuth, SEMP, …) | **Compile-time blocked by the `BrokerSource` interface.** |
| Pool-internal access to `pool.clients` | Convention + helper (`clientFor`/`setClient`) + doc comments. Same-package — Go cannot block this. (Unchanged.) |

The story is now fully consistent: every place we identified a "rule in the contributor's head" risk has been moved to "rule in the type system" — except the one Go cannot mechanically enforce (same-package access to `pool.clients`), which keeps its convention-based protection per the Go-language limitations documented in §3.3a.

### 13.8 Why we missed this in §12

Same shape of retrospective as §12.5. When fixing the aliased-map problem in §12, we picked the simplest replacement (`*config.ServerConfig`) without auditing whether the new dependency was wider than needed. The interface narrowing only became visible when a reviewer asked: *"Why does the pool need to maintain a reference to ServerConfig? It has many other details too."*

Generalizable lesson, building on §12.5: **when you replace one dependency with another, audit the new dependency's surface for the same risks you were fixing.** Otherwise you're trading one shape of over-coupling for another.

### 13.9 Why this isn't infinite-regress

A reasonable concern after §12 and §13: are we going to keep finding "one more thing to fix" forever?

No. The two corrections share a single root cause — the original plan didn't audit the pool's data model when introducing the new accessor API. §12 fixed the redundant-map problem. §13 fixes the over-broad-replacement problem. These are two sides of the same audit oversight, surfaced in two passes only because we caught them in two separate reviews.

The pool now has exactly the dependency surface it needs: one HTTP client cache it owns, plus a narrow interface to ask for broker configs. There's no further "what about this other field?" question to surface, because the interface explicitly enumerates the full dependency. The corrections terminate here.

### 13.10 Why `sempCfg` is not also narrowed to an interface

The pool holds two references into `ServerConfig`: a narrow `BrokerSource` interface (broker resolution) and a wide `*config.SEMPConfig` pointer (SEMP knobs forwarded to downstream clients). The asymmetry is deliberate; the reasoning has two parts.

**First — what the pool actually does with each:**

- **Broker side:** the pool reads broker configs to construct clients. The methods do real work (case-folding via `strings.ToLower`, display-form projection, sort). Worth encapsulating regardless of access concerns.
- **SEMP side:** the pool does NOT read any SEMP field. It stores the pointer and forwards it to `NewBrokerClient`, where downstream layers (`resilience/transport.go`, `resilience/sender.go`, `sempv1/client.go`, `sempv2/client.go`) read the fields they need. Narrowing the pool's SEMP access to an interface would be theatrical — barring access to fields nobody at the pool layer reads.

**Second — what's on the other side of each pointer:**

- `*config.ServerConfig` contains auth credentials (`ClientAuth.DevToken`), the server port, the log level, TLS file paths, and broker definitions. A pool method reaching into any of those except brokers/SEMP is a layering violation. The rejected surface is **large AND contains categorically wrong-layer state.**
- `*config.SEMPConfig` contains six SEMP-related fields. Same neighborhood as the pool's role (broker network behavior). No credentials, no wrong-layer concerns. The rejected surface is **small AND categorically same-layer.**

**Honest framing:** this is a judgment call, not a mechanical application of the principle. The principle ("narrow access to what's used; protect against accidental over-coupling") doesn't pick the line by itself; the calibration ("narrow when the rejected surface is large AND categorically wrong-layer") does. Same principle, different surfaces, different outputs.

**Revisit if:** `SEMPConfig` gains fields the pool shouldn't see (credentials, lifecycle hooks, etc.), OR the pool starts reading SEMP fields directly rather than forwarding the pointer.

## 14. Third mid-implementation correction — remove the pool's `configFor`/`clientFor`/`setClient` helpers

This section was added after a PR review by Andrea Ross flagged that the pool helpers carry an implicit lock requirement which would be easy for a future contributor to miss.

### 14.1 What we found

Andrea's observation:

> *`clientFor` and `setClient` don't take `p.mu` themselves. Today the only caller is `getOrCreate`, which holds the right lock at each call site, so this is fine. But the helpers are now named, documented, and visible inside the package as the obvious way to touch `p.clients`. A future contributor adding, say, a `HasClient(alias string) bool` method who reaches for `p.clientFor(alias)` will introduce a data race that won't surface except under load and a `-race` run.*

The helpers we added in §5.2 made the trap *easier* to fall into, not harder. The §3.3 principle was to prefer type-system enforcement over discipline; named, reusable-looking helpers with implicit lock requirements violated that principle by inviting use of a surface whose safety property lives off the call site.

### 14.2 Alternatives considered

| Option | Verdict |
|---|---|
| Docstring only | Weak; relies on the contributor reading the docs |
| `Locked` suffix rename (Go stdlib convention) | Defensible only with a second caller to disambiguate; here there was just one |
| `TryLock`-based runtime assertion | Catches "forgot lock entirely" but adds runtime cost |
| `clientCache` wrapper type owning the lock | Strongest, but over-engineered for a two-method state today |
| **Inline the helpers into `getOrCreate`** | Removes the trap entirely; no helpers means no misuse |

Self-locking helpers were ruled out structurally: Go's `sync.RWMutex` is not reentrant, and the double-checked locking pattern in `getOrCreate` requires the read and the write to happen under the *caller's* lock.

### 14.3 Decision

Inline `configFor`, `clientFor`, and `setClient` into `getOrCreate`. Remove all three. The "chokepoint for case-folding" justification from §5.2 was speculative; `getOrCreate` is the only function that touches `p.clients`, so there was no second caller for the helpers to factor.

What stays:

- The `clients` field doc is now a concise statement of the two operational invariants — case-folding (`strings.ToLower` before indexing) and lock discipline (`p.mu` for all access) — plus the realized-vs-configured distinction between `p.clients` and `p.src`.
- `BrokerSource` interface stays — it solves a structurally different problem (§14.4).

### 14.4 Why `BrokerSource` survives but the pool helpers don't

The two abstractions look superficially similar but are solving different problems:

| | `BrokerSource` (kept) | `clientFor`/`setClient` (removed) |
|---|---|---|
| Touches mutable state? | No (`cfg.brokers` is immutable post-startup) | Yes (`p.clients` is mutated by `setClient`, read by `clientFor`) |
| Requires caller discipline? | No — methods are self-contained | Yes — caller must hold `p.mu` |
| Enforcement | Type-level narrowing (compile-time) | Convention (docstring) |
| Misuse mode? | None | Forget the lock, race ships silently |

`BrokerSource` shrinks the *type surface* the pool can see (compile-time). The helpers shrank only *call-site verbosity* (no compile-time effect). The helpers had a failure mode that ships silently; `BrokerSource` has no failure mode the compiler doesn't catch. That's the line — not stylistic neatness, but whether the abstraction has a misuse mode.
