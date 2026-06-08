# Releasing

We release when the gates are green, not on a calendar. Every change that clears the gates is publishable the same day.

This document describes the target release model. Each item is marked **[Implemented]** (enforced in CI today) or **[Planned]** (the direction we're building toward).

## Versioning

One axis describes a release: its **version**. We use [SemVer 2.0.0](https://semver.org) (`MAJOR.MINOR.PATCH`), and the pre-release identifier carries maturity:

| Version | Maturity | Promotes when |
|---------|----------|---------------|
| `0.4.0-alpha.N` | early, unstable; surface may still change | feature-complete for the release and no known P0/P1 bugs |
| `0.4.0-beta.N` | feature-complete, stabilizing | clears the Stable gate (soaked on beta, no new P0/P1) |
| `0.4.0` | stable, supported | — |

A new release line starts at `alpha`. Promotion drops or advances the pre-release identifier; the `MAJOR.MINOR.PATCH` core only changes when the change itself warrants it. Pre-1.0, breaking changes bump MINOR. Tags are immutable **[Implemented]** and signed **[Planned]**.

## Distribution pointers

Fixed tags name one exact build and never move:

| Tag | Resolves to | Status |
|-----|-------------|--------|
| `{version}` | the exact version, e.g. `0.4.0` or `0.4.0-beta.1` | [Implemented] |
| `:sha` | the commit the build came from | [Implemented] |

Moving pointers let consumers track a stream instead of a fixed version:

| Pointer | Resolves to | Status |
|---------|-------------|--------|
| `{major}.{minor}` | newest stable patch of that minor, e.g. `0.4` | [Implemented] |
| `:latest` | newest **stable** version | [Planned] — today `:latest` moves to the newest tag of *any* maturity, including pre-releases |
| `:edge` | newest tag of any maturity (release-on-green) | [Planned] |
| `:alpha`, `:beta` | newest tag at that stage | [Planned] |

## Gates

**Publish gate** — automated, runs on every change. Clearing it makes the build publishable as a pre-release:

- CI green — lint, build, `go vet`, `go test -race`, three E2E suites (basic MCP, OAuth, monitoring) **[Implemented]**
- Security scans clean — FOSSA SCA (dependencies, licenses) **[Implemented]**
- No open P0/P1 bugs **[Planned]**
- Eval harness passes **[Planned]**
- Coverage threshold met **[Planned]**
- No performance regression **[Planned]**
- Release notes drafted — today GitHub auto-generates notes at release time **[Planned]**

**Stable gate** — promotes a candidate to a stable version (drops the pre-release suffix):

- Publish gate re-verified on the candidate **[Planned]**
- No new P0/P1 found since the candidate **[Planned]**
- Release notes finalized **[Planned]**
- Immutable tag **[Implemented]**, signed **[Planned]**

Today a stable release clears the **Publish gate** only; the remaining Stable-gate criteria are **[Planned]**.

## Cutting a release **[Implemented]**

Releases are tag-triggered. To cut a stable release:

```bash
git tag v0.4.0          # SemVer, prefixed with v; omit the suffix for stable
git push origin v0.4.0
```

Pushing the tag runs `.github/workflows/release.yml`, which:

1. Re-runs the full build-and-test suite.
2. Runs the FOSSA scan against the tag.
3. Builds binaries for `linux` and `darwin` × `amd64` and `arm64`.
4. Builds and pushes a multi-arch image to `ghcr.io/solacedev/solace-broker-mcp` (`{version}`, `{major}.{minor}`, `latest`, `sha` tags).
5. Publishes a GitHub Release with auto-generated notes, the binary archives, and SHA-256 checksums.

Anyone with permission to push tags can cut a release. The release succeeds only if every job passes; a failed gate blocks publication.

Pre-release tags (`v0.4.0-beta.1`) and the `:edge`/`:alpha`/`:beta` pointers follow the same workflow once continuous pre-release publishing is wired up **[Planned]**.

## Rollback **[Implemented]**

Tags are immutable — we roll forward, not back.

- **Bad release:** fix on the default branch, then tag the next PATCH (e.g. `v0.4.1`). The fix follows the same gates, and the new tag moves the moving pointers forward to the good build.
- **Steer users away:** mark the bad GitHub Release as a pre-release or delete it. The tag and artifacts remain for auditability; pin-by-version and pin-by-digest consumers are unaffected.

## DORA metrics **[Planned]**

Pre-release-vs-stable is what makes the numbers mean something, and it's already in the version, so nothing extra is tracked:

| Metric | Measured on |
|--------|-------------|
| Deployment frequency | all tags |
| Lead time for changes | all tags |
| Change failure rate | stable versions |
| Time to restore (MTTR) | stable versions |

Releasing on green keeps deployment frequency high and lead time under a day; the gated promotion to a stable version protects failure rate and MTTR. The split becomes meaningful once continuous pre-release publishing exists — until then, every tag is a stable version and the two populations are the same.
