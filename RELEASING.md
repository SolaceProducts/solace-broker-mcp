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

A new release line starts at `alpha`. Promotion drops or advances the pre-release identifier; the `MAJOR.MINOR.PATCH` core only changes when the change itself warrants it. Pre-1.0, breaking changes bump MINOR. Tags are immutable by convention; CI enforcement is **[Planned]** — today nothing prevents retagging, and a force-updated tag would simply re-run the release workflow. Signed tags are **[Planned]**.

## Distribution pointers

Fixed tags name one exact build and never move:

| Tag | Resolves to | Status |
|-----|-------------|--------|
| `{version}` | the exact version, e.g. `0.4.0` or `0.4.0-beta.1` | [Implemented] |
| `sha-<short-sha>` | the commit the build came from, e.g. `sha-860c190` | [Implemented] |

Moving pointers let consumers track a stream instead of a fixed version:

| Pointer | Resolves to | Status |
|---------|-------------|--------|
| `{major}.{minor}` | newest stable patch of that minor, e.g. `0.4` | [Implemented] |
| `:latest` | newest **stable** version | [Planned] — today `:latest` moves to the newest tag of *any* maturity, including pre-releases |
| `:edge` | newest tag of any maturity (release-on-green) | [Planned] |
| `:alpha`, `:beta` | newest tag at that stage | [Planned] |

## Gates

**Publish gate** — automated. Clearing it makes the build publishable as a pre-release:

- CI green — lint, build, `go vet`, `go test -race`, five E2E suites (basic MCP, OAuth, monitoring, management, action); runs on pull requests and on pushes to `main`, not on every branch push **[Implemented]**
- Security scans clean — FOSSA SCA (dependencies, licenses); runs on same-repo pull requests, default-branch pushes, and release tags. A fork pull request gets no scan, because GitHub withholds the credential — see `.github/ADMIN_SETUP.md` **[Implemented]**
- No open P0/P1 bugs **[Planned]**
- Eval harness passes **[Planned]**
- Coverage threshold met **[Planned]**
- No performance regression **[Planned]**
- Release notes drafted — the GitHub Release body is minted from that version's `CHANGELOG.md` block, with the auto-generated PR list appended beneath; a missing block fails the release **[Implemented]**

**Stable gate** — promotes a candidate to a stable version (drops the pre-release suffix):

- Publish gate re-verified on the candidate **[Planned]**
- No new P0/P1 found since the candidate **[Planned]**
- Release notes finalized **[Planned]**
- Immutable tag **[Planned]** — convention today, not enforced — signed **[Planned]**

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
4. Builds and pushes a multi-arch image to `ghcr.io/solacedev/solace-broker-mcp` (`{version}`, `{major}.{minor}`, `latest`, `sha-<short-sha>` tags).
5. Publishes a GitHub Release whose notes are the tagged version's `CHANGELOG.md` block (with the auto-generated PR list appended beneath), plus the binary archives and SHA-256 checksums. If no `## [X.Y.Z]` block exists for the tag, the release fails rather than falling back to auto-only notes.

Anyone with permission to push tags can cut a release.

The jobs are not fully serialized:

```
push v* tag
  ├─> test (reuses build-and-test.yml)
  │     ├─> build-binaries (matrix: 4 OS/arch) ──┐
  │     └─> build-docker (pushes the image) ─────┼─> release (GitHub Release)
  └─> fossa_scan ─────────────────────────────────┘
```

A failed job blocks the GitHub Release, binaries, and checksums. The container image is the exception: it is pushed as soon as build-and-test passes, in parallel with the FOSSA scan, so a FOSSA failure can leave the image and its moving pointers already published on `ghcr.io`. Gating the image push on every job is **[Planned]** — until then, if a release run fails partway, check `ghcr.io` and roll forward (see Rollback).

Pre-release tags (`v0.4.0-beta.1`) and the `:edge`/`:alpha`/`:beta` pointers follow the same workflow once continuous pre-release publishing is wired up **[Planned]**.

## Release runbook

The manual steps around the automated workflow.

> The `/cut-release` skill (`.claude/skills/cut-release/SKILL.md`) automates this runbook
> step-for-step — promote `[Unreleased]` in a prepare-release PR, tag the merge commit, verify the
> run. Prefer it over doing these by hand; the steps below are the contract it follows and the
> manual fallback.

Before tagging:

1. Confirm `main` is green: `gh run list --branch main --limit 1`.
2. Update `CHANGELOG.md` on `main`: move the `[Unreleased]` items into a new dated `## [X.Y.Z]` version section and update the comparison links at the bottom. This is **required** — the release workflow extracts that block as the Release body and fails the release if the dated block is absent. Do it in a "prepare release" PR *before* tagging, so the block lives in the tagged commit.
3. Regenerate the third-party license inventory from the toolchain: `go-licenses report ./cmd/server` supplies the module, version, and license data for `THIRD_PARTY_LICENSES.md`. Reconcile it against the FOSSA scan, which remains the authoritative check.

After pushing the tag:

1. Watch the run: `gh run list --workflow=release.yml --limit 1`; on failure, `gh run view <run-id> --log`.
2. Verify the release: `gh release view <tag>` shows four binary archives, `checksums-sha256.txt`, and the curated CHANGELOG notes (with the PR list appended); the image tags are present on `ghcr.io/solacedev/solace-broker-mcp`.
3. Spot-check a binary: download the archive for your platform, verify it (`shasum -a 256 -c checksums-sha256.txt --ignore-missing`), and run `./solace-broker-mcp --version` — it prints the tag.
4. Announce once verified: internal channels, and the [Solace Community](https://solace.community/) for releases worth a wider note.

If a job fails for environmental reasons, re-run it: `gh run rerun <run-id>`. Never delete and re-push a tag to retry — tags are immutable (see Versioning). If the build itself is bad, fix on `main` and tag the next PATCH (see Rollback).

## Rollback **[Implemented]**

Tags are never reused — we roll forward, not back.

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
