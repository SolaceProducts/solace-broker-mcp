---
name: refresh-third-party-inventory
description: Fix a Dependabot PR's red "Third-party licenses current" check — checks out the PR branch, runs `make refresh-third-party-inventory`, shows the diff, and pauses for explicit confirmation before committing (DCO sign-off) and pushing. Never pushes unprompted. Triggers on "fix the licenses check on this PR", "the Dependabot PR is red on third-party licenses", or a pointer to a failing `Third-party licenses current` run.
user_invocable: true
---

# Refresh third-party inventory

Run `make refresh-third-party-inventory` (SOL-152956) against a Dependabot PR and land the
result, with a human checkpoint before anything gets pushed. Invoked with
`/refresh-third-party-inventory <PR number or URL>`.

Dependabot bumps Go modules and re-pins GitHub Action SHAs but cannot run post-update scripts, so
its own PRs land with `THIRD_PARTY_LICENSES.md` / `THIRD_PARTY_BUILD_TEST.md` unmodified and
`Third-party licenses current` red. `make refresh-third-party-inventory` already turns fixing that
into one command; this skill is that command asked for in plain language instead of remembered and
typed by hand.

**The one checkpoint is the diff, before anything is written.** This skill checks out a real PR
branch and pushes a real commit to it — Step 5 always stops and shows the diff, and committing and
pushing (Step 6) only happen after that's explicitly confirmed. One confirmation covers both, the
same way `/changelog` never commits and `/add-logs` never applies without being shown first. There
is no "just push it if it looks fine" mode.

## When to use

- A Dependabot PR is red on `Third-party licenses current` and needs the routine fix.
- Someone points at a failing run of that check and asks for it to be fixed.
- Not for a PR failing for any other reason — see Step 2.
- Not for a fork pull request — see Step 3.

## Usage

- `/refresh-third-party-inventory <PR number>` — e.g. `/refresh-third-party-inventory 321`
- `/refresh-third-party-inventory <PR URL>`
- `/refresh-third-party-inventory <failing run URL>` — resolve to its PR first
- No argument: ask which PR or run to target before doing anything else.

**Precondition:** `gh auth status` first. Every step below needs an authenticated `gh`.

## Steps

### Step 1: Resolve the target PR

From a PR number or URL, use it directly.

From a run URL (`https://github.com/<owner>/<repo>/actions/runs/<run-id>`): parse `<owner>/<repo>`
and `<run-id>` from the URL itself, then
`gh api repos/<owner>/<repo>/actions/runs/<run-id> --jq '.pull_requests[0].number'`. `gh run view
--json` has no field that names the PR or even the head repository directly (`headRepository` is
not a valid field for it at all), so this is not a shortcut — it is the only reliable way to get
from a run to its PR via the API. If `pull_requests` is empty (the run wasn't triggered by a pull
request — a push to `main`, for instance), say so and ask which PR to target rather than guessing.

### Step 2: Confirm this is actually inventory drift

`gh pr checks <PR>` and look at its output for `Third-party licenses current` specifically — the
command itself exits nonzero whenever *any* check on the PR is failing, which is the expected,
normal case here, not a sign the command failed. If `Third-party licenses current` is green, or if
the PR's only failures are unrelated (lint, a real test failure, an E2E flake), **stop and say so**
— this skill fixes one specific class of failure, not "make the PR green." Running
`make refresh-third-party-inventory` against a PR with nothing to refresh is harmless on a clean
checkout of `main` (the target no-ops cleanly there), but running it and reporting success on a PR
that's red for a different reason would still be misleading.

### Step 3: Confirm it's safe to check out, then do it

Three checks, in order, before touching anything:

1. **Same repository.** `gh pr view <PR> --json isCrossRepository,author` — if `isCrossRepository`
   is true, **stop**: this is a fork pull request, out of scope for this skill (a fork's
   `GITHUB_TOKEN` can't push back to it the way this skill's own `git push` assumes, and a
   Dependabot PR is never a fork PR in this repo anyway). Note if `author` isn't `dependabot[bot]`
   — not a hard stop, since a human's own PR can hit the same drift, but worth surfacing so the
   person running this knows it's outside the common case.
2. **Clean working tree.** `git status --porcelain` first, and record a way back:
   `git branch --show-current`, falling back to `git rev-parse HEAD` when that's empty (a detached
   HEAD, where `--show-current` prints nothing). If the tree isn't clean, **stop and ask** rather
   than switching anyway — this working directory may be shared with other work in progress
   (another session, another task) that a branch switch would disrupt or that could get carried
   onto the PR's branch unintentionally. Do not stash-and-hope; surface the situation and let a
   human decide.
3. **Check it out.** `gh pr checkout <PR>`. If this fails — most likely because the PR's branch is
   already checked out in another worktree, which this repo's own worktree-based workflow makes a
   real, ordinary case, not a corner one — **stop and report the exact error**. Do not force a
   checkout or improvise a workaround; a colliding worktree usually means someone (or another
   agent) is already using that branch.

Once checked out, the branch's remote tracking is already set up — the push in Step 6 needs
nothing further, for exactly the same-repository case Check 1 above just confirmed.

### Step 4: Run the refresh

`make refresh-third-party-inventory` from the repo root.

- **If `make` itself fails because the target doesn't exist** ("No rule to make target..."): this
  means SOL-152956 (which ships the target) hasn't reached this branch — say so plainly and stop.
  This is not the same thing as the refusal case below; don't describe it as "the script refused."
- **If it exits 0 with no file changes:** the inventory already matches. Report that, restore the
  original branch (Step 8), and stop — the check is failing for a reason this skill doesn't cover
  (back to Step 2's judgment call).
- **If it exits 0 with changes:** proceed to Step 5.
- **If it exits nonzero for any other reason:** the underlying scripts refused to guess at
  something rather than write a fix they couldn't verify — see
  `.github/scripts/refresh-licenses-inventory.sh` and `refresh-build-test-inventory.sh`'s own
  header comments for the current, authoritative list of what that covers; don't rely on a copy of
  that list here, since it can change as those scripts do. **Do not try to hand-fix it yourself.**
  Surface the script's own error message verbatim. The two refresh scripts write per row, not
  atomically per run, so a refusal partway through can still leave one file changed on disk even
  though the overall exit code is nonzero — if `git diff` shows anything, show it, so the person
  reading this sees exactly what state the branch is in. Then **stop, leaving the tree exactly as
  it is, on the PR's branch** — do not proceed to Step 8, since its checkout requires a clean tree
  and this state generally isn't one.

### Step 5: Show the diff and stop

`git diff`. Present it clearly — which file(s) changed, which rows, old value → new value.
**Wait for explicit confirmation before proceeding.** "Looks right" or equivalent from the person
who asked for this is the checkpoint; do not infer consent from silence or move on unprompted.

### Step 6: Commit and push, only after confirmation

- `git add THIRD_PARTY_LICENSES.md THIRD_PARTY_BUILD_TEST.md` (only these two — never `git add -A`;
  the refresh touches nothing else, and blindly staging everything could pick up unrelated local
  changes the person running this doesn't intend to commit).
- Commit with a fixed, predictable message and both trailers:
  ```
  git commit -s -m "Refresh third-party inventory" -m "Generated-by: Claude Code (/refresh-third-party-inventory)"
  ```
  `-s` for the DCO trailer every commit on this repo needs — this runs under the invoking person's
  own git identity; no bot identity or separate credential is needed, because a person is the one
  asking for and reviewing this, unlike the fully-automated path (a separate, not-yet-built
  mechanism) that would need one. `Generated-by:` records that an agent produced the diff, which
  matters here specifically because this is a license-compliance artifact — provenance of an edit
  to it is itself worth keeping.
- `git push`. **Never force-push.** If the push is rejected as non-fast-forward — Dependabot can
  force-push its own branch between this skill's checkout and its push — stop and report that
  rather than retrying with `--force`; the branch moved out from under this run and needs a human
  to decide what to do next, not another automatic guess.

### Step 7: Report back

Link the PR. Note that `Third-party licenses current` will re-run automatically now that the branch
has a new commit — no further action needed unless it goes red again for a different reason.

### Step 8: Restore the original branch

Only reached from the full happy path (Step 4's "exits 0 with changes" branch, through Steps 5–7) —
the two early-stop cases in Step 4 already say to do this themselves, at the point they stop,
rather than falling through here.

Confirm `git status --porcelain` is clean first (it should be, right after a push) before
switching — this step must not run against a dirty tree, same reasoning as Step 3. Then:

```
git checkout <original-branch-or-commit-from-step-3>
git branch -D <the-PR-branch-gh-pr-checkout-created>
```

matching how `/cut-release` cleans up its own temporary branch at the point it's done with it.
Leave the working directory as it was found — don't leave it sitting on the PR's branch, or on a
stray local branch for it, once the task is done.

## Rules

- Never push, or commit, without the human first seeing and confirming the diff in Step 5.
- Never hand-author a fix for anything the refresh scripts themselves refused (Step 4) — surface
  their error and stop.
- Never `git add -A` or otherwise stage more than the two inventory files.
- Never force-push. A rejected push is a stop-and-report condition, not a retry-with-force one.
- Never run this against a fork pull request (Step 3) or a PR whose failure isn't
  `Third-party licenses current` (Step 2).
- If `make refresh-third-party-inventory` doesn't exist on the checked-out branch (an older PR,
  or SOL-152956 not yet merged there), say so and stop rather than trying to reconstruct its steps
  by hand.
- Never switch branches into a dirty working tree — confirm it's clean first (Step 3), and confirm
  it's clean again before restoring the original branch (Step 8). A refusal that leaves a file
  partially changed (Step 4) means Step 8 is skipped entirely, not run against that state.
