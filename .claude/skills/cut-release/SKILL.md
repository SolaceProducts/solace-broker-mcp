---
name: cut-release
description: Cut a release end-to-end — promote CHANGELOG [Unreleased] into a dated version block (auto-drafting any gap entries via /changelog), open the prepare-release PR, watch for its merge, then push the vX.Y.Z tag that triggers the release pipeline. The human's one checkpoint is reviewing/merging the changelog PR; the tag fires automatically once it merges. Triggers on "cut a release" / "cut vX.Y.Z".
user_invocable: true
---

# Cut a release

Run the whole release from one command. Invoked with `/cut-release <version>` (or "cut a new
v0.6.0 release").

The release pipeline (`.github/workflows/release.yml`) triggers on a pushed `v*` tag and does the
heavy lifting itself — builds binaries for 4 OS/arch, pushes the multi-arch image, extracts the
tagged commit's `## [X.Y.Z]` CHANGELOG block, and **creates the GitHub Release** from it with
artifacts + checksums attached. No GitHub-UI "draft release / auto-generate notes" step is needed.
So cutting a release reduces to: get the dated CHANGELOG block onto `main`, then push the tag.

**The one human checkpoint is merging the changelog PR** — that is where "make sure the changelog
is updated properly" happens, and it is required because `main` is protected and the pipeline reads
the CHANGELOG from the *tagged commit*. Everything before and after is automatic: the merge is
treated as the go-ahead, and the tag fires on merge (guarded — see Phase D).

## Usage

- `/cut-release <version>` — e.g. `/cut-release 0.6.0` or `v0.6.0`.

**Validate first:** reject anything not matching `^v?[0-9]+\.[0-9]+\.[0-9]+$` (stable SemVer) before
any command uses the value. Then normalize once and derive both forms explicitly: strip a leading
`v` for the **bare** form (`0.6.0`) used in the `## [0.6.0]` heading and wherever the Phase D awk
takes `v="0.6.0"`; keep the **`v`-prefixed** form (`v0.6.0`) for the git tag and compare-URL
segments. Never pass the raw argument through un-normalized (a `v0.6.0` input must not yield
`## [v0.6.0]` or a `vv0.6.0` tag).

**Preconditions:** `gh` must be authenticated — run `gh auth status` up front; if it fails, stop and
tell the operator to run `gh auth login`. Every phase below depends on `gh`.

## Phase A — Prepare the changelog

**A1. Sync and preconditions.** First `git fetch origin main --tags` so every check below runs
against fresh remote state, not a stale local `main`. Warn if the working tree is dirty.

`vPREV` (for the compare range) is the highest **stable** tag strictly *below* the target version —
not the global newest tag. Filter out pre-release tags (those containing `-`) and any tag ≥ the
target, then take the max. Plain `git tag --sort=-v:refname | head -1` is wrong for a backport/patch
on an older line (it would pick a newer minor) and floats a `-beta` tag above stable. If no stable
tag below the target exists, this is the first release — stop and confirm the bootstrap range with
the operator.

**Idempotency / resume — decide from REMOTE state, not the working tree.** A resumed run (fresh
clone, or `main` checked out after a dropped session) must not re-promote or open a duplicate PR.
Check, in order:
- `vX.Y.Z` already on the remote (`git ls-remote --tags origin "vX.Y.Z"`) → release already cut; go
  to Phase E (reporting only).
- `## [X.Y.Z]` block already on `origin/main` (via the Phase D extraction) but no remote tag → the
  PR merged; **jump straight to Phase D** and cut the tag.
- An open PR from `release/vX.Y.Z` (`gh pr list --head "release/vX.Y.Z" --state open`) → resume the
  Phase C watch on it.
- A `release/vX.Y.Z` branch exists (local or remote) but no PR → push it and open the PR (Phase B).
- None of the above → start clean at A2.

**A2. Gap detection + auto-draft.** Enumerate PRs merged since `vPREV`
(`gh pr list --base main --state merged --json number,title,mergedAt,headRefName,files`, kept if
merged after `git log -1 --format=%cI vPREV`; fall back to `git log vPREV..main --format=%s%n%b`
if `gh` is unavailable — degrade, don't fail). Keep only PRs hitting **production surface**, using
the exact filter from `.claude/hooks/changelog-reminder.sh`:
```
grep -E '^(internal/config/|internal/tools/|internal/composite/definitions/tools\.yaml)' \
  | grep -v '_test\.go$'
```
Cross-check each qualifying PR's `SOL-XXXXX` against the `Tracked under SOL-…` trailers in
`[Unreleased]`. For each gap, invoke the **`/changelog`** skill (Skill tool) with that PR's range to
draft the missing entry into `[Unreleased]`. **Halt the whole flow if any draft left a `SOL-????`
placeholder** — surface it for the human to resolve before anything is promoted or pushed.

**A3. Version.** If the version was passed, use it. Otherwise suggest a bump from the `[Unreleased]`
contents per `RELEASING.md` (pre-1.0: any `- **BREAKING**:` → MINOR; new **Added** → MINOR; only
**Fixed**/**Security** → PATCH) and confirm with the human before continuing.

**A4. Promote (deterministic).** Edit `CHANGELOG.md` (today = `date +%F`):
- Rename `## [Unreleased]` → `## [X.Y.Z] - <today>`.
- Insert a fresh `## [Unreleased]` heading above it (heading only, no empty category stubs).
- In `## Links`: repoint `[Unreleased]` to `…/compare/vX.Y.Z...HEAD` and add
  `[X.Y.Z]: …/compare/vPREV...vX.Y.Z` beneath it, matching the existing URL shape.
- Touch nothing else — no existing bullets, and not the bottom `## Release Process` / `##
  Versioning` sections.

**A5. Verify the promotion.** The release gate only checks that the version block is non-empty, so
assert the rest by hand before proceeding:
- `.github/scripts/extract-release-notes.sh vX.Y.Z /tmp/release-notes-dryrun.md` → non-empty
  `## [X.Y.Z]` block.
- A fresh `## [Unreleased]` heading still exists above the dated block (dropping it silently breaks
  the next cycle's gap detection and the changelog hook).
- The `## Links` block has a well-formed `[Unreleased]: …/compare/vX.Y.Z...HEAD` and a new
  `[X.Y.Z]: …/compare/vPREV...vX.Y.Z` line.

If any check fails, the promotion is malformed — fix before proceeding.

## Phase B — Open the prepare-release PR

- Branch off `main`: `git switch -c release/vX.Y.Z` (never commit release changes on `main`).
- Commit only `CHANGELOG.md`: `Prepare release vX.Y.Z`, ending the message with the
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer.
- Push and open the PR with `gh pr create`, base `main`, body ending with the
  `🤖 Generated with [Claude Code](https://claude.com/claude-code)` trailer. Summarize the
  promotion (version, categories, any gap entries drafted), and **state explicitly that merging this
  PR automatically tags and publishes `vX.Y.Z`** — an immutable public GitHub Release + `ghcr.io`
  image — so the reviewer knows the merge is the release go-ahead, not just a docs edit. Note the
  reminder to regenerate `THIRD_PARTY_LICENSES.md` (`go-licenses report ./cmd/server`) if
  dependencies changed.
- Print the PR URL and tell the human: review + merge to make the changelog official; the release
  fires automatically on merge.

## Phase C — Watch for the merge

- Poll the PR every ~30s: `gh pr view <n> --json state,mergedAt,mergeCommit`.
- On `MERGED` → **record the `mergeCommit` SHA** and carry it into Phase D. That exact commit is what
  gets tagged — do not re-derive it from the branch tip.
- On `CLOSED` (unmerged) → stop; the release is cancelled.
- Bound the wait (e.g. ~30 min of polling). On timeout, stop and report: "PR #N is still open at
  <url> — nothing was tagged; re-run `/cut-release <version>` to resume." A dropped session is safe:
  the A1 remote-state check resumes cleanly (merged-but-untagged → Phase D).

## Phase D — Cut the release (guarded auto-tag)

The merge is the go-ahead, but still verify the hard gates before pushing. `MERGE_SHA` is the
`mergeCommit` captured in Phase C (on a resumed run with none in hand, get it from
`gh pr view <n> --json mergeCommit`).

- `git fetch origin main --tags`.
- **Tag-exists guard — distinguish remote from local-only.** If `vX.Y.Z` is on the remote
  (`git ls-remote --tags origin "vX.Y.Z"` non-empty) → the release was already cut; stop and roll
  forward with the next PATCH (tags are immutable). If it exists **locally only** (`git tag --list`
  matches but the remote does not) → a previous push failed *after* tagging; do **not** roll forward
  — just re-run `git push origin vX.Y.Z` to finish, then go to Phase E.
- **Verify `origin/main` carries the dated block** — extract from the *remote* branch and refuse if
  empty (the promotion did not actually merge). Substitute the **bare** version for `v=` (e.g.
  `v="0.6.0"`, never `v0.6.0`):
  ```
  git show origin/main:CHANGELOG.md | awk -v v="0.6.0" '
    BEGIN { gsub(/\./, "\\.", v) }
    $0 ~ ("^## \\[" v "\\]") { p = 1; print; next }
    p && /^## \[/            { exit }
    p                        { print }
  '
  ```
- **Confirm the tagged commit is releasable — scoped to the commit, not the branch.** Verify
  `MERGE_SHA` is contained in `origin/main` (`git merge-base --is-ancestor <MERGE_SHA> origin/main`),
  then check the `build-and-test` run for that exact commit:
  `gh run list --workflow=build-and-test.yml --commit <MERGE_SHA> --json status,conclusion`. Treat
  `in_progress`/`queued` as **wait and re-poll** — right after a merge the run is normally still
  running, which is not a failure. Proceed only on `conclusion == success`; stop and surface only on
  an actual failure conclusion.
- **Tag the exact merged commit** (not the branch tip — `origin/main` may have advanced since the
  merge) and push:
  ```
  git tag vX.Y.Z <MERGE_SHA>
  git push origin vX.Y.Z
  ```

## Phase E — Report and hand off

- **Confirm the release run actually started for this tag** — don't assume. Resolve the run for
  `vX.Y.Z` (`gh run list --workflow=release.yml` filtered to the tag / `MERGE_SHA`); if none appears
  shortly after the push, flag it — tag protection or a disabled workflow can swallow the trigger.
- Report the run URL and its conclusion. **On failure**, point the operator at `RELEASING.md`
  Rollback and warn that the container image and its moving pointers (`:latest`, `{major}.{minor}`)
  may already be published on `ghcr.io` even though a later job failed — recovery is roll-forward to
  the next PATCH, never a retag.
- **On success**, print the `RELEASING.md` "After pushing the tag" checklist: verify
  `gh release view vX.Y.Z` shows four archives + `checksums-sha256.txt` + the curated notes,
  spot-check a binary's `--version`, and announce once verified.
- Stop. Do not retry the run or edit the Release.

## Rules

- One human gate only: reviewing/merging the changelog PR. Never merge the PR yourself.
- Never invent a SOL ticket number — halt on any `SOL-????` placeholder (Phase A2).
- Reuse the exact production-surface filter from `.claude/hooks/changelog-reminder.sh` — keep it in
  sync with that hook and `.github/scripts/changelog-check.sh` (the CI gate) if the surface changes.
- Only edit the `[Unreleased]` heading, the new dated block, and the `## Links` lines in
  `CHANGELOG.md`. Never rewrite existing bullets or the bottom `## Release Process` / `##
  Versioning` sections.
- Never reuse or force-update a tag; never tag when the tagged commit lacks the dated block on
  `origin/main` or its `build-and-test` run is not `success` (Phase D).
- Tag the exact merge commit captured in Phase C, verified to be an ancestor of `origin/main` —
  never the branch tip or local `HEAD`.
- Do not modify the `/changelog` skill — invoke it unchanged for gap drafting.
