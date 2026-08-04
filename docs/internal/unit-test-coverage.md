# Unit Test Coverage Gate

CI fails the `build` job if repo-wide unit test coverage drops below **80%** of
statements. This document states the policy and why it's shaped this way.

## What's measured

`go test -race -coverprofile=coverage.out ./...`, then the aggregate `total:`
line from `go tool cover -func=coverage.out` — every package in the module,
package-aggregate (statement-weighted), no per-package thresholds.

## No exclusion list

The gate has **no excluded packages or paths** — `cmd/server` (server
bootstrap/wiring, largely exercised by the Docker E2E suites rather than unit
tests) and small test-helper packages consumed only by other packages' tests
(e.g. `internal/oauth/cache/cachetest`, `internal/composite/postprocess/postprocesstest`)
are counted like everything else.

This was a deliberate decision, not an oversight. Two things made it easy:

1. Repo-wide aggregate coverage measured 88.6% at the time this gate was
   added — comfortably above 80% with `cmd/server` (37.7%) and the two
   test-helper packages (0%, ~110 lines combined) included as-is. No
   exclusion was needed to pass.
2. An exclusion list is itself a place for untested code to hide later — the
   ticket that established this gate (SOL-150787) explicitly calls out that
   coverage "must not be gamed by padding." Keeping the denominator as
   "every statement in the module" is the simplest rule that can't quietly
   grow loopholes as packages are added or renamed.

If a future package genuinely can't be meaningfully unit-tested (e.g. it's
pure `main`-style wiring exercised only by E2E), the right move is to keep it
thin and push logic into a tested package underneath it, not to add it to an
exclusion list. Revisit this policy if a legitimate case shows up where that
isn't possible.

## Running locally

```bash
make test-cover
```

Matches the CI step exactly, including the 80% threshold check.
