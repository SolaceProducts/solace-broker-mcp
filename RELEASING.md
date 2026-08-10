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

A new release line starts at `alpha`. Promotion drops or advances the pre-release identifier; the `MAJOR.MINOR.PATCH` core only changes when the change itself warrants it. Pre-1.0, breaking changes bump MINOR. Tags are immutable by convention; CI enforcement is **[Planned]** — today nothing prevents retagging, and a force-updated tag would simply re-run the release workflow. Signed *git tags* are **[Planned]** — distinct from the published artifacts, which do carry build provenance attestations **[Implemented]** (see Gates).

## Distribution pointers

Fixed tags name one exact build and never move:

| Tag | Resolves to | Status |
|-----|-------------|--------|
| `{version}` | the exact version, for example, `0.4.0` or `0.4.0-beta.1` | [Implemented] |
| `sha-<short-sha>` | the commit the build came from, for example, `sha-860c190` | [Implemented] |

Moving pointers let consumers track a stream instead of a fixed version:

| Pointer | Resolves to | Status |
|---------|-------------|--------|
| `{major}.{minor}` | newest stable patch of that minor, for example, `0.4` | [Implemented] |
| `:latest` | newest **stable** version | [Planned] — today `:latest` moves to the newest tag of *any* maturity, including pre-releases |
| `:edge` | newest tag of any maturity (release-on-green) | [Planned] |
| `:alpha`, `:beta` | newest tag at that stage | [Planned] |

## Gates

**Publish gate** — automated. Clearing it makes the build publishable as a pre-release:

- CI green — lint, build, `go vet`, `go test -race`, five E2E suites (basic MCP, OAuth, monitoring, management, action); runs on pull requests and on pushes to `main`, not on every branch push **[Implemented]**
- Security scans clean — FOSSA SCA (dependencies, licenses); runs on same-repo pull requests, default-branch pushes, and release tags. A fork pull request gets no scan, because GitHub withholds the credential — see `.github/ADMIN_SETUP.md` **[Implemented]**
- `THIRD_PARTY_LICENSES.md` and `NOTICE` match the binary — `.github/scripts/licenses-check.sh` fails when the inventory drifts from `go list -deps ./cmd/server`, or when a dependency's NOTICE is not propagated. It runs on every pull request as `Third-party licenses current`, and again at the tag as a `needs` of `build-binaries` and `build-docker`, so nothing publishes from a drifted state. The tag-time run is what makes this a guarantee: the pull-request check is not yet in the required-status-check list (see `.github/ADMIN_SETUP.md`), so until it is, a PR can merge with it red **[Implemented]**
- No open P0/P1 bugs **[Planned]**
- Eval harness passes **[Planned]**
- Coverage threshold met **[Planned]**
- No performance regression **[Planned]**
- Release notes drafted — the GitHub Release body is minted from that version's `CHANGELOG.md` block, with the auto-generated PR list appended beneath; a missing block fails the release **[Implemented]**

**Stable gate** — promotes a candidate to a stable version (drops the pre-release suffix):

- Publish gate re-verified on the candidate **[Planned]**
- No new P0/P1 found since the candidate **[Planned]**
- Release notes finalized **[Planned]**
- Immutable tag **[Planned]** — convention today, not enforced
- Git tag signed by the tagger **[Planned]** — a separate mechanism from artifact attestation below, and still unenforced
- Artifacts attested **[Implemented]** — the release workflow attaches a GitHub build provenance attestation to each binary archive and to the container image. With `--signer-workflow` and `--source-digest` (see the runbook below) a consumer can prove the artifact was built by this repository's `release.yml` from a named commit. `--repo` on its own is weaker than it looks: it binds only the repository, so any workflow in it holding `id-token: write` and `attestations: write` could mint an attestation that passes

Today a stable release clears the **Publish gate** only; the remaining Stable-gate criteria are **[Planned]**.

## Cutting a release **[Implemented]**

Releases are tag-triggered. To cut a stable release:

```bash
git tag v0.4.0          # SemVer, prefixed with v; omit the suffix for stable
git push origin v0.4.0
```

Pushing the tag runs `.github/workflows/release.yml`, which:

1. Re-runs the full build-and-test suite.
2. Runs the release readiness check (the Guardian gate) against the tag.
3. Builds binaries for `linux` and `darwin` × `amd64` and `arm64`, attesting each archive's build provenance in the job that built it.
4. Builds and pushes a multi-arch image to `ghcr.io/solaceproducts/solace-broker-mcp` (`{version}`, `{major}.{minor}`, `latest`, `sha-<short-sha>` tags), and attests the image digest, pushing the attestation to the registry alongside it.
5. Publishes a GitHub Release whose notes are the tagged version's `CHANGELOG.md` block (with the auto-generated PR list appended beneath), plus the binary archives and SHA-256 checksums. If no `## [X.Y.Z]` block exists for the tag, the release fails rather than falling back to auto-only notes.

Anyone with permission to push tags can cut a release.

The jobs are not fully serialized:

```
push v* tag
  ├─> test               (reuses build-and-test.yml)
  ├─> release-notes      (CHANGELOG block must exist)
  ├─> licenses           (inventory must match the binary)
  └─> release-readiness  (Guardian gate)

waits on
  build-binaries   test, release-notes, licenses
  build-docker     test, release-notes, licenses, release-readiness   ← pushes the image
  release          build-binaries, build-docker, release-readiness
```

A failed job blocks the GitHub Release, binaries, checksums, and the container image: `build-docker` waits on the readiness check as well as the build, so a failing Guardian gate publishes nothing. That matters because a registry push cannot be withdrawn — the binaries are only artifacts until `release` publishes them, but the image is live the moment it is pushed.

One window remains: the image is pushed *before* it is attested, so a run that fails on the attest step leaves `latest` live with no attestation — a consumer's `gh attestation verify` then fails because the release is incomplete, not because the image was tampered with. That ordering is unavoidable, because attesting a registry digest requires the digest to exist. If a release run fails partway, check `ghcr.io` and roll forward (see Rollback).

Pre-release tags (`v0.4.0-beta.1`) and the `:edge`/`:alpha`/`:beta` pointers follow the same workflow once continuous pre-release publishing is wired up **[Planned]**.

## Release runbook

The manual steps around the automated workflow.

> The `/cut-release` skill (`.claude/skills/cut-release/SKILL.md`) automates this runbook
> step-for-step — promote `[Unreleased]` in a prepare-release PR, tag the merge commit, verify the
> run. Prefer it over doing these by hand; the steps below are the contract it follows and the
> manual fallback.

Before tagging:

1. Confirm `main` is green: `gh run list --branch main --limit 1`.
2. Update `CHANGELOG.md` on `main`: move the `[Unreleased]` items into a new dated `## [X.Y.Z]` version section and update the comparison links at the bottom. This step is **required** — the release workflow extracts that block as the Release body and fails the release if the dated block is absent. Do it in a "prepare release" PR *before* tagging, so the block lives in the tagged commit.
3. Regenerate the third-party license inventory from the toolchain: `go-licenses report ./cmd/server` supplies the module, version, and license data for `THIRD_PARTY_LICENSES.md`. Reconcile it against the FOSSA scan, which remains the authoritative check.

After pushing the tag:

1. Watch the run: `gh run list --workflow=release.yml --limit 1`; on failure, `gh run view <run-id> --log`.
2. Verify the release: `gh release view <tag>` shows four binary archives, `checksums-sha256.txt`, and the curated CHANGELOG notes (with the PR list appended); the image tags are present on `ghcr.io/solaceproducts/solace-broker-mcp`.
3. Spot-check a binary: download the archive for your platform, verify it (`shasum -a 256 -c checksums-sha256.txt --ignore-missing`), and run `./solace-broker-mcp --version` — it prints the tag.
4. Verify the attestations on both artifact kinds. `--signer-workflow` and `--source-digest` are what make this a check rather than a look: without them the command binds only the repository, and its output names the build and signer workflow but never prints a commit SHA, so there is nothing to eyeball. With them, a wrong builder or a wrong source commit fails the command.

   Set `TAG` and `PLATFORM`, then paste the rest as-is. The image reference deliberately uses `VERSION`, not `TAG`: `docker/metadata-action`'s `{{version}}` pattern strips the leading `v`, so the image tags are `0.7.0`, `0.7`, `latest`, and `sha-<short-sha>` — there is no `v0.7.0` image tag.

   ```bash
   TAG=v0.7.0                  # the tag you pushed
   PLATFORM=linux-amd64        # the archive you downloaded in step 3
   VERSION="${TAG#v}"          # image tag: the same, without the leading v
   COMMIT="$(git rev-list -n1 "$TAG")"

   gh attestation verify "solace-broker-mcp-${TAG}-${PLATFORM}.tar.gz" \
     --repo SolaceProducts/solace-broker-mcp \
     --signer-workflow SolaceProducts/solace-broker-mcp/.github/workflows/release.yml \
     --source-digest "${COMMIT:?no commit resolved for $TAG - git fetch --tags, then check the tag name}"

   gh attestation verify "oci://ghcr.io/solaceproducts/solace-broker-mcp:${VERSION}" \
     --repo SolaceProducts/solace-broker-mcp \
     --signer-workflow SolaceProducts/solace-broker-mcp/.github/workflows/release.yml \
     --source-digest "${COMMIT:?no commit resolved for $TAG - git fetch --tags, then check the tag name}"
   ```

   The `${COMMIT:?…}` guard is load-bearing, not decoration. `gh` accepts an empty `--source-digest` instead of rejecting it, so an unresolved `COMMIT` would silently drop the commit constraint and still print `✓ Verification succeeded!` — the guard aborts the `gh` command with a visible message instead. It does not close an interactive shell.

   Both commands fetch the attestation from the GitHub API. To verify the copy stored beside the image on `ghcr.io` instead, add `--bundle-from-oci` to the second one.
5. Announce once verified: internal channels, and the [Solace Community](https://solace.community/) for releases worth a wider note.

If a job fails for environmental reasons, re-run it: `gh run rerun <run-id>`. Never delete and re-push a tag to retry — tags are immutable (see Versioning). If the build itself is bad, fix on `main` and tag the next PATCH (see Rollback).

## Rollback **[Implemented]**

Tags are never reused — we roll forward, not back.

- **Bad release:** fix on the default branch, then tag the next PATCH (for example, `v0.4.1`). The fix follows the same gates, and the new tag moves the moving pointers forward to the good build.
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
