# In-process integration tests

This directory holds Go tests that compose multiple components from `internal/`
and exercise them through their public APIs. They run as part of the normal
`go test ./...` invocation — no Docker, no real brokers, no separate make
target (today). The directory itself is the marker that distinguishes them
from unit tests.

## How this tier differs from the others

| Tier | Lives in | Runs via | Uses |
|---|---|---|---|
| Unit | `internal/<pkg>/*_test.go` | `go test` | One component, sometimes a fake HTTP server |
| **Integration (this dir)** | `test/integration/*_test.go` | `go test` | Multiple `internal/` components composed; fake brokers via `httptest.NewServer` |
| E2E | `test/e2e-basic-mcp/`, `test/oauth/`, `test/e2e-monitoring/` | Shell scripts + Docker | Real binaries, real Solace brokers (or Keycloak), real network |

## When a test belongs here

Use this tier when the test exercises **at least two `internal/` components
composed together**, and the property being asserted only makes sense when
they are wired up — typically routing, isolation, concurrency, or end-to-end
data flow across a non-trivial graph.

Examples that belong here:
- Pool routing under concurrent first-touch
- Cross-broker credential isolation
- Token cache + Exchanger + Authenticator interaction (T5/T6)
- Full token-exchange round-trip with a fake IdP (T7a)

Examples that do **not** belong here — keep them as unit tests:
- "Authenticator returns the right header for these inputs"
- "Pool returns ErrUnknownAlias for an unknown name"
- "TokenCache get/put round-trips a value"

The rule: if you can write the test using only one component's public surface,
it is a unit test and belongs next to that component's source. If the
assertion would be meaningless without the second component wired in, it
belongs here.

## Naming convention

**One file per qualitatively distinct invariant.** Name the file after the
invariant, not after the components being composed or the test setup.

Why: a file named after components (`pool_auth_test.go`) tempts every test
that touches those components, and we end up with a 2000-line grab bag.
A file named after the invariant (`broker_credential_isolation_test.go`)
has a sharp scope; tests that don't fit it move to a new file.

When adding a new isolation property, **prefer a new file** over expanding an
existing one unless the assertions truly share machinery. The folder is the
marker; cheap-to-create new files are the structure.

### Existing files

- `broker_credential_isolation_test.go` — credentials configured for one
  broker never reach another broker's wire, under concurrent load. Static
  modes only (basic, bearer).

### Anticipated future files (see SOL-150070 decomposition)

- `oauth_user_isolation_test.go` — different OAuth users targeting the same
  broker get different tokens; cache keys partition correctly; singleflight
  collapses same-user requests only.
- `oauth_broker_isolation_test.go` — different brokers using OAuth do not
  share tokens or cookies, even though they share the global Exchanger.
- `oauth_token_lifecycle_test.go` — cache eviction on 401, concurrent
  re-exchange does not produce stale-token reuse.

## Running

```
go test -race ./test/integration/...
```

`-race` is the point of running these — they exist precisely because the
underlying property only fails under concurrency. Add `-v` to see the
`t.Logf` evidence emitted by each test.

## Future cleanup

When the integration suite grows large enough that its runtime matters in CI,
the files in this directory can be gated with `//go:build integration` and
run via a dedicated `make integration` target. The directory structure is
already correct for that upgrade; no file moves required.

Not blocking SOL-150070. File a follow-up ticket when the runtime cost
warrants it.
