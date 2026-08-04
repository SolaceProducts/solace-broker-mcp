# Unit Test Coverage Gate

CI fails the `build` job if repo-wide unit test coverage drops below **85%** of
statements. This document states the policy, what the gate can and can't
catch, and why it's shaped this way.

## What's measured

`go test -race -coverprofile=coverage.out ./...`, then the aggregate `total:`
line from `go tool cover -func=coverage.out` — every package in the module,
package-aggregate (statement-weighted), no per-package thresholds.

## What this gate is — and isn't

This is a **backstop against collapse, not a regression detector.** At 85%
against a measured actual of 88.6%, roughly 130 statements of headroom exist
before the gate trips — enough to catch something wholesale (a large new
package landing with no tests, a careless mass-deletion of test files) but
nowhere near tight enough to catch a small, localized regression in one
package. The aggregate is a single number computed over the entire module; a
handful of untested branches in one package is a rounding error against the
total, even though it's a real gap in that package.

This isn't hypothetical: while auditing the test suite for this same PR, two
branches in `ListKafkaReceivers`/`ListKafkaSenders` (skip-on-malformed-row,
truncation-flag passthrough) were left untested by a test trim — about 8
statements total. The aggregate gate never came close to firing; the
regression was found by diffing **per-package** coverage percentages before
and after the change, not by anything CI enforced. That's the precision this
aggregate number cannot offer, structurally — tightening the floor further
doesn't fix it, since even a much stricter aggregate can't isolate which
package lost coverage or by how much.

**What actually catches a regression like that:** comparing per-package
`go test -cover ./...` output before and after a change, as part of review —
not the aggregate gate. Do this whenever a PR touches or removes existing
tests; it's cheap (the same command this gate already runs) and it's the
tool with the resolution the aggregate lacks.

## No exclusion list

The gate has **no excluded packages or paths** — `cmd/server` (server
bootstrap/wiring, largely exercised by the Docker E2E suites rather than unit
tests) and small test-helper packages consumed only by other packages' tests
(e.g. `internal/oauth/cache/cachetest`, `internal/composite/postprocess/postprocesstest`)
are counted like everything else.

This was a deliberate decision, not an oversight. Two things made it easy:

1. Repo-wide aggregate coverage measured 88.6% at the time this gate was
   added — comfortably above the floor with `cmd/server` (37.7%) and the two
   test-helper packages (0%, ~110 lines combined) included as-is. No
   exclusion was needed to pass.
2. An exclusion list is itself a place for untested code to hide later — the
   ticket that established this gate (SOL-150787) explicitly calls out that
   coverage "must not be gamed by padding." Keeping the denominator as
   "every statement in the module" is the simplest rule that can't quietly
   grow loopholes as packages are added or renamed. This was stress-tested
   directly: a throwaway ~40-statement package with no test file and no
   importers still showed up in the profile and pulled the aggregate down
   proportionally, confirming `-coverprofile` without `-coverpkg` does count
   untested packages rather than silently ignoring them.

If a future package genuinely can't be meaningfully unit-tested (e.g. it's
pure `main`-style wiring exercised only by E2E), the right move is to keep it
thin and push logic into a tested package underneath it, not to add it to an
exclusion list. Revisit this policy if a legitimate case shows up where that
isn't possible.

## Ratcheting the floor

85% is a starting point pinned just below actual, not a permanent target. As
real coverage climbs, raise the floor to stay a few points under it — the
gate is only as useful as the gap between it and reality is small. Don't
raise it to sit flush against actual; a small buffer avoids CI going red on
an incidental, defensible dip (e.g. a legitimately hard-to-unit-test error
branch) that isn't the kind of collapse this gate exists to catch.

## Running locally

```bash
make test-cover
```

Matches the CI step exactly, including the 85% threshold check.
