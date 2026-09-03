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

**Preconditions:** run `gh auth status` up front. Phases B–E (open/watch the PR, tag, report)
**require** an authenticated `gh` — if it's unavailable, stop and have the operator run
`gh auth login` before those phases. Phase A2 alone can degrade to a `git log` fallback when `gh` is
missing (less precise — see A2), so preparation may proceed unauthenticated, but the release cannot
be cut without `gh`.

## Phase A — Prepare the changelog

**A1. Sync and preconditions.** First `git fetch origin main --tags` so every step runs against
fresh remote state. Warn if the working tree is dirty. **The promotion must be based on
`origin/main`, not the local checkout** — `git fetch` updates the `origin/main` ref but not your
local `main`/working tree, so a local `main` that's behind origin would make A4 promote a stale
`CHANGELOG.md` and silently drop `[Unreleased]` entries merged since. The release branch is
therefore created *from the fetched remote tip* (see the clean-start path below), and A2–A4 run on
that branch.

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
- None of the above → create the release branch from the fresh remote tip
  (`git switch -c release/vX.Y.Z origin/main`) so promotion runs on the latest `main`, then start at A2.

**A2. Gap detection + auto-draft.** Enumerate PRs merged since `vPREV`:
```
gh pr list --base main --state merged --limit 500 \
  --json number,title,mergedAt,headRefName,files
```
The explicit `--limit 500` is **required** — `gh`'s default is 30, and a release window wider than
that would silently drop older merged PRs and miss their gaps (the exact failure this phase exists to
catch). Keep those merged after `vPREV`'s commit time, compared **in UTC** — `gh`'s `mergedAt` is
UTC (`…Z`) but `git --format=%cI` carries a local offset (`-04:00`), so a raw lexical compare across
the two is wrong. Get the cutoff as a UTC instant with
`TZ=UTC0 git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd vPREV` (portable — avoids
BSD-vs-GNU `date(1)` parsing differences), then string-compare the two `…Z` timestamps. If `gh` is
unavailable, fall back to `git log vPREV..origin/main --format=%s%n%b` — this degrades gracefully but
is less precise, so prefer the authenticated `gh` path (see Preconditions). Keep only PRs hitting **production surface**, filtered
with the single source of truth shared by the reminder hook and CI gate — source it, never re-embed
the pattern:
```
source .github/scripts/production-surface.sh
grep -E "$SURFACE_RE" | grep -v "$SURFACE_TEST_EXCLUDE"
```

**Confirm a real gap before drafting — do not draft from SOL presence alone.** A surface-regex
match is a *candidate*, not a gap; applied literally the naive "one `/changelog` per unmatched SOL"
rule over-fires (on v0.6.0 it flagged 11 candidates for 1 true gap, and auto-drafting all 11 would
have produced duplicate/garbage entries and 10 wasted `/changelog` runs). A **true gap** is *a
production-surface PR whose user-facing contract change has no representation in `[Unreleased]`*.
For each candidate, in order, and skip it (no `/changelog`) as soon as one applies:
- **Umbrella entries.** A PR's `SOL-XXXXX` absent from the `Tracked under SOL-…` trailers is **not**
  a gap if its effect is already captured under an umbrella entry (one `Added`/`Changed` bullet that
  rolls up several sub-tickets of one feature and lists their SOLs). Match against the delivered
  contract change, not a 1:1 SOL-to-bullet mapping.
- **Delivered ticket, not follow-ups.** Match the ticket the PR *delivers*. SOLs also appear as
  inline future-work references (e.g. `[Unreleased]` mentions a follow-up SOL) — a body-mention match
  is a false signal; use the PR's own tracked ticket.
- **`tools.yaml` prose-only edits.** A candidate that matches `SURFACE_RE` *only* through
  `internal/composite/definitions/tools.yaml` needs a second gate: inspect the diff for a changed
  tool **name / param / output / step-key**. If it changed only description or example prose (no
  contract change), it is not a gap — skip it (it belongs to `no-changelog`).
- **Diff, don't guess.** Before drafting, read the candidate PR's diff and confirm an actual
  name/param/output/behavior change with no representation in `[Unreleased]`. Decide from the diff,
  never from SOL presence alone.

For each candidate that survives all four checks, invoke the **`/changelog`** skill (Skill tool)
with that PR's range to draft the missing entry into `[Unreleased]`. **Halt the whole flow if any
draft left a `SOL-????` placeholder** — surface it for the human to resolve before anything is
promoted or pushed.

**Nothing to release? Stop before promoting.** After gap detection and any drafting, if `[Unreleased]`
still has no entry line (`^- `), there is nothing to ship since `vPREV`. Do not promote an empty block
(it would only fail A5 and the release gate downstream). Report "nothing to release since vPREV" and
stop — and because A1's clean-start path already created `release/vX.Y.Z` (with no commits yet), leave
no litter behind: `git switch - && git branch -D release/vX.Y.Z`.

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

**A5. Verify the promotion.** The release gate now refuses both a *missing* section and a
heading-only one (no `- ` entry), but it does not check the `[Unreleased]`/`## Links` structure — so
assert the rest by hand before proceeding:
- `.github/scripts/extract-release-notes.sh vX.Y.Z /tmp/release-notes-dryrun.md` succeeds (it fails
  fast if the block is missing or entry-less) **and** the extracted notes contain at least one real
  entry, not just the `## [X.Y.Z]` heading.
- A fresh `## [Unreleased]` heading still exists above the dated block (dropping it silently breaks
  the next cycle's gap detection and the changelog hook).
- The `## Links` block has a well-formed `[Unreleased]: …/compare/vX.Y.Z...HEAD` and a new
  `[X.Y.Z]: …/compare/vPREV...vX.Y.Z` line.

If any check fails, the promotion is malformed — fix before proceeding.

**A6. Draft the human-readable release-notes summary.** The published GitHub Release should be
*skimmable*, not the verbatim CHANGELOG block. The pipeline runs on the tag
with no LLM available, so the summary must be authored here and committed in the release PR:
`.github/scripts/extract-release-notes.sh` publishes `.github/release-notes/vX.Y.Z.md` when present
and otherwise falls back to the raw block. Write that file:
- Draft **from the promoted `## [X.Y.Z]` block only** — condense each entry to 1–2 sentences,
  grouped by the same categories (`Added`/`Changed`/`Fixed`/`Security`/…), dropping rationale and
  implementation detail. The CHANGELOG remains the source of truth.
- Do not introduce facts absent from the block; do not restate the release gate — the script still
  enforces "the CHANGELOG section exists and has real entries" independently of this file.

## Phase B — Open the prepare-release PR

- The `release/vX.Y.Z` branch was already created from `origin/main` in A1 (never commit release
  changes on `main` itself) — confirm you're on it with the promotion staged.
- Commit `CHANGELOG.md` and the `.github/release-notes/vX.Y.Z.md` summary from A6 (nothing else):
  `Prepare release vX.Y.Z`, ending the message with the
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer.
- Push and open the PR with `gh pr create`, base `main`, body ending with the
  `🤖 Generated with [Claude Code](https://claude.com/claude-code)` trailer. Summarize the
  promotion (version, categories, any gap entries drafted), and **state explicitly that merging this
  PR automatically tags and publishes `vX.Y.Z`** — an immutable public GitHub Release + `ghcr.io`
  image — so the reviewer knows the merge is the release go-ahead, not just a docs edit.
- **`THIRD_PARTY_LICENSES.md` reminder — only when deps actually changed.** Detect with
  `git diff --stat vPREV..HEAD -- go.mod go.sum`; if it reports changes, include the reminder to
  regenerate via `go-licenses report ./cmd/server` and **name the changed modules** (from the
  `go.mod` diff). If `go.mod`/`go.sum` are untouched, stay silent — do not emit the reminder.
- Print the PR URL and tell the human: review + merge to make the changelog official; the release
  fires automatically on merge.

## Phase C — Watch for the merge

This wait is gated on a human merging the PR, so it is unbounded from the skill's side — do **not**
burn it in a foreground ~30s poll loop (each cycle reloads full context and is the bulk of the
skill's own churn/latency). Instead launch **one blocking watch loop as a background command** — via
your runner's background execution (e.g. the Bash tool's background mode), not a shell `&` inside the
snippet — so it sleeps between checks and wakes the model once, on a terminal state, not every cycle:
Extract the state with `--jq` (don't grep the raw JSON — `gh` only emits compact JSON when piped,
so a literal `"state":"MERGED"` match is fragile), and bound the loop mechanically (~30 min at 30s):
```
tries=0
while :; do
  state=$(gh pr view <n> --json state --jq '.state')
  if [ "$state" = "MERGED" ]; then
    MERGE_SHA=$(gh pr view <n> --json mergeCommit --jq '.mergeCommit.oid'); break
  fi
  [ "$state" = "CLOSED" ] && { echo "closed unmerged — release cancelled"; break; }
  tries=$((tries + 1))
  [ "$tries" -ge 60 ] && { echo "PR #<n> still open after ~30 min — stop; re-run to resume"; break; }
  sleep 30
done
```
- On `MERGED` → `MERGE_SHA` holds the `mergeCommit` oid; carry it into Phase D. That exact commit is
  what gets tagged — do not re-derive it from the branch tip.
- On `CLOSED` (unmerged) → stop; the release is cancelled.
- On the ~30 min bound → stop and report: "PR #N is still open at <url> — nothing was tagged; re-run
  `/cut-release <version>` to resume." A dropped session is safe: the A1 remote-state check resumes
  cleanly (merged-but-untagged → Phase D).

## Phase D — Cut the release (guarded auto-tag)

The merge is the go-ahead, but still verify the hard gates before pushing. `MERGE_SHA` is the
`mergeCommit` captured in Phase C (on a resumed run with none in hand, get it from
`gh pr view <n> --json mergeCommit`).

- `git fetch origin main --tags`.
- **Tag-exists guard — distinguish remote from local-only.** If `vX.Y.Z` is on the remote
  (`git ls-remote --tags origin "vX.Y.Z"` non-empty) → this version is already released; tags are
  immutable, so do **not** re-tag — go to Phase E and report the existing release (matching A1's
  resume behavior). Shipping further changes needs a *new* version, i.e. a fresh `/cut-release`. If
  the tag exists **locally only** (`git tag --list` matches but the remote does not) → a previous
  push failed *after* tagging; re-run `git push origin vX.Y.Z` to finish, then go to Phase E.
- **Verify `origin/main` carries the dated block** — extract from the *remote* branch and refuse if
  the block is empty **or contains no entry line (`^- `)**. The CI release gate now rejects an
  entry-less block too, but that fires only *after* the tag is pushed; catching it here — before an
  immutable tag exists — avoids a doomed run, and an empty/entry-less block usually means the
  promotion did not actually merge.
  Substitute the **bare** version for `v=` (e.g. `v="0.6.0"`, never `v0.6.0`). The awk whole-record
  reference is written `$(0)` — identical to `$0` in awk, but with no literal `$0` token for a
  positional-argument substitution to turn into the version string, and without the `\$0` escaping
  that awk itself rejects as a syntax error. Keep the parentheses:
  ```
  git show origin/main:CHANGELOG.md | awk -v v="0.6.0" '
    BEGIN { gsub(/\./, "\\.", v) }
    $(0) ~ ("^## \\[" v "\\]") { p = 1; print; next }
    p && /^## \[/              { exit }
    p                          { print }
  '
  ```
- **Confirm the tagged commit is releasable — scoped to the commit, not the branch.** Verify
  `MERGE_SHA` is contained in `origin/main` (`git merge-base --is-ancestor <MERGE_SHA> origin/main`),
  then capture the `build-and-test` run id for that exact commit and block on it once (don't
  foreground-poll):
  ```
  run_id=$(gh run list --workflow=build-and-test.yml --commit <MERGE_SHA> \
             --json databaseId --jq '.[0].databaseId')
  gh run watch "$run_id" --exit-status   # blocks to completion; exit 0 = success, non-zero = failure
  ```
  Right after a merge the run is normally still `in_progress`/`queued` — that is not a failure;
  `gh run watch` blocks until it finishes (run it backgrounded), expected ~5–10 min. Proceed only on
  a zero exit; stop and surface only on an actual failure.
- **Tag the exact merged commit** (not the branch tip — `origin/main` may have advanced since the
  merge) and push:
  ```
  git tag vX.Y.Z <MERGE_SHA>
  git push origin vX.Y.Z
  ```

## Phase E — Report and hand off

- **Confirm the release run actually started for this tag** — don't assume. Resolve the run for
  `vX.Y.Z` (`gh run list --workflow=release.yml` filtered to the tag / `MERGE_SHA`). Distinguish two
  states so the check doesn't false-alarm: a run **record that exists but sits in `queued`** is
  normal — it is waiting on a runner/environment, not swallowed, so wait on it. Only when **no run
  record appears at all** within ~2 min of the push should you flag it — tag protection or a disabled
  workflow can swallow the trigger.
- Once a run exists, capture its id and block on it once, rather than a foreground poll loop:
  ```
  run_id=$(gh run list --workflow=release.yml --commit <MERGE_SHA> --json databaseId --jq '.[0].databaseId')
  gh run watch "$run_id" --exit-status
  ```
  Run it backgrounded; expected ~15–25 min end-to-end, and it may sit in `queued` before starting.
- Report the run URL and its conclusion. **On failure**, point the operator at `RELEASING.md`
  Rollback and warn that the container image and its moving pointers (`:latest`, `{major}.{minor}`)
  may already be published on `ghcr.io` even though a later job failed — recovery is roll-forward to
  the next PATCH, never a retag.
- **On success**, print the `RELEASING.md` "After pushing the tag" checklist: verify
  `gh release view vX.Y.Z` shows four archives + `checksums-sha256.txt` + the curated notes,
  spot-check a binary's `--version`, and announce once verified.
- **Optional cleanup (offer, don't force).** After a successful release the local checkout is often
  still on the merged `release/vX.Y.Z` branch and the remote branch lingers. Offer to tidy up:
  `git switch main && git pull --ff-only origin main`, then delete the merged branch local + remote
  (`git branch -d release/vX.Y.Z`; `git push origin --delete release/vX.Y.Z`). Skip silently if the
  branch was already deleted on merge.
- Stop. Do not retry the run or edit the Release.

## Rules

- One human gate only: reviewing/merging the changelog PR. Never merge the PR yourself.
- Never invent a SOL ticket number — halt on any `SOL-????` placeholder (Phase A2).
- Source the production-surface pattern from `.github/scripts/production-surface.sh` — the single
  source of truth shared with the reminder hook and CI gate. Never re-embed the regex.
- Only edit the `[Unreleased]` heading, the new dated block, and the `## Links` lines in
  `CHANGELOG.md`. Never rewrite existing bullets or the bottom `## Release Process` / `##
  Versioning` sections.
- Never reuse or force-update a tag; never tag when the tagged commit's `origin/main` block is
  missing or entry-less, or its `build-and-test` run is not `success` (Phase D).
- Tag the exact merge commit captured in Phase C, verified to be an ancestor of `origin/main` —
  never the branch tip or local `HEAD`.
- Do not modify the `/changelog` skill — invoke it unchanged for gap drafting.
