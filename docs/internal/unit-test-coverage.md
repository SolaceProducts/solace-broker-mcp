# Unit Test Coverage Gate

CI fails the `build` job if repo-wide unit test coverage drops below **85%** of
statements. This document states the policy, what the gate can and can't
catch, and why it's shaped this way.

## What's measured

`go test -race -coverprofile=coverage.out ./...`, then the aggregate `total:`
line from `go tool cover -func=` over that profile with the module's
`test/...` harness packages filtered out — package-aggregate
(statement-weighted), no per-package thresholds. Everything under `cmd/` and
`internal/` counts; see [One exclusion](#one-exclusion-test-harness-packages)
for what the filter drops and why.

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

## One exclusion: test-harness packages

The gate drops exactly one thing from the denominator: packages under the
module's top-level `test/` directory. Everything else counts, including the
cases most likely to tempt an exclusion — `cmd/server` (server
bootstrap/wiring, largely exercised by the Docker E2E suites rather than unit
tests, 37.2%) and the test-helper packages consumed only by other packages'
tests (`internal/oauth/cache/cachetest`,
`internal/composite/postprocess/postprocesstest`, 0%, ~110 lines combined).

The line is **shipped code vs. the apparatus that tests it**, not "hard to
test." `test/performance/` holds standalone binaries — a load generator, a
mock SEMP server, a fidelity differ, a memory sampler — that exist only to
drive a perf harness and never enter a release artifact. They are explicitly
experimental and not for production use, with no compatibility or support
guarantees; flags and output formats there change without notice
(`test/performance/README.md` carries the same disclaimer). Coverage of them
says nothing about the risk this gate is meant to bound, but at ~2,150
statements they moved the aggregate 88.3% → 72.9% purely by landing, which
would have forced the floor down or forced tests onto throwaway tooling.

Two constraints kept this from becoming the open-ended exclusion list the
original policy warned about, and both should hold for any future change here:

1. **It's a categorical rule, not a list.** One anchored path prefix, no
   per-package entries to accumulate. There's nothing to append to when the
   next package is inconvenient — widening it means arguing that some other
   whole category isn't shipped code, in review, in this file.
2. **It can't reach production packages.** The filter is anchored on the full
   module path (`github.com/SolaceProducts/solace-broker-mcp/test/`), so a
   package merely *named* like a test helper — `cachetest`,
   `postprocesstest` — stays in the denominator. Only top-level `test/` is
   affected.

The original anti-padding intent from SOL-150787 is unchanged: coverage still
must not be gamed, and the denominator is still every statement of shipped
code in the module. For a package that *is* shipped and genuinely can't be
meaningfully unit-tested (pure `main`-style wiring exercised only by E2E), the
answer remains what it was — keep it thin and push logic into a tested package
underneath it, not extend this exclusion.

Note that `test/e2e-common/broker-driver` is a separate Go module and was
never in `./...` to begin with; CI vets it in its own step.

## Ratcheting the floor

85% is a starting point pinned just below actual, not a permanent target. As
real coverage climbs, raise the floor to stay a few points under it — the
gate is only as useful as the gap between it and reality is small. Don't
raise it to sit flush against actual; a small buffer avoids CI going red on
an incidental, defensible dip (for example, a legitimately hard-to-unit-test
error branch) that isn't the kind of collapse this gate exists to catch.

## Running locally

```bash
make test-cover
```

Matches the CI step exactly, including the 85% threshold check. It writes two
profiles: `coverage.out` (everything, unfiltered — inspect this one) and
`coverage.gate.out` (the filtered profile the threshold is computed over).

For the per-package view that actually catches a localized regression:

```bash
go test -cover ./...
```
