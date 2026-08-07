# Repository Admin Reference

How this repository is configured, and what is still outstanding. Everything here
needs **repository admin access**, and several items need org-admin access.

This is a public repository. Its settings are the controls that stand between an
external contribution and `main`, so the sections below record both what is set and
*why*, and re-reading the live value is preferred over trusting a table.

**Admin needed**: @solace-aross or repository administrators

---

## Outstanding actions

Everything else in this document describes configuration that is already in place.
These are the open items, each with the section that explains it.

| Action | Ticket | Why it is still open |
|--------|--------|----------------------|
| **Enable private vulnerability reporting**, and settle who watches the Security tab | SOL-152967 | Not enabled. Reports through it do not reach the `support@solace.com` queue `.github/SECURITY.md` publishes — see [Security Settings](#security-settings) |
| **Confirm fork pull request workflow approval by hand** | SOL-152960 | **No REST API exposes this setting.** It cannot be verified by tooling; someone with org-admin access has to read the radio button — see [Fork pull request workflows](#fork-pull-request-workflows) |
| **Make CodeQL a required check** | SOL-152858 | `Analyze (go)` passes on every pull request but is not in the ruleset, so a finding does not block a merge. The same ticket has to establish which configuration owns the check — see [Security Settings](#security-settings) |
| **Pin the three unpinned required contexts** | — | `Guardian scan gate`, `DCO sign-off`, and `DCO check self-test` carry no `integration_id` — see [Required status checks](#required-status-checks) |
| **Cut a release that covers `main`** | — | `main` is 130 commits ahead of v0.6.0 (read 2026-08-07) and `[Unreleased]` carries two BREAKING entries, so the latest release does not describe this repository — see [Releases](#releases) |
| **Repoint "ask a question" links at the project's Discourse category** | SOL-152413 | Blocked on the category existing. Nothing is broken meanwhile; the links resolve to `https://solace.community/` — see [Features](#features-m2-support-channels) |
| **Close the fork pull request scanning gap** | — | A fork pull request gets no supply-chain verdict at all. Needs an owner, not just a fix — see [What the supply-chain gate actually guarantees](#what-the-supply-chain-gate-actually-guarantees) |

---

## Repository Settings

### Basic Information (M1: Repository Metadata)

Navigate to: **Settings → General → About**

Description and topics already hold values, so these are edits, not blanks.
Currently: description "Production MCP server for the Solace broker", topics
`ai-agents`, `go`, `mcp`, `solace`, website unset. Replace them with the below, or
decide deliberately to keep what is there.

**Description** (appears at top of repo page):
```
MCP (Model Context Protocol) server for Solace brokers - manage queues, topics, and configuration via Claude Code and other AI agents
```

**Website** (appears in sidebar):
```
https://solace.com
```

**Topics** (click ⚙️ next to About):
```
mcp
model-context-protocol
solace
broker
semp
ai-agent
claude
golang
infrastructure
messaging
event-driven
pubsub
```

**Social Preview Image** (optional):
- Upload a preview image for social media sharing
- Recommended size: 1280x640px
- Should include Solace branding + "MCP Server" text

---

## Features (M2: Support Channels)

Navigate to: **Settings → General → Features**

**Do not enable GitHub Discussions.** Support runs on two channels so the
conversation sits where Solace users already are, rather than splitting across a
second forum. This supersedes the earlier plan to open Discussions.

| Channel | Use for | Where |
|---------|---------|-------|
| GitHub Issues | Bug reports, feature requests | This repository |
| Solace Community | Questions, ideas, general discussion | https://solace.community/ |
| Private disclosure | Security vulnerabilities | `.github/SECURITY.md` |

**Settings to set:**
- ⬜ **Discussions** - leave OFF (currently off; keep it that way)
- ✅ Issues (already enabled)
- ✅ Preserve this repository (mark as important)
- ✅ Sponsorships (if applicable)

**No `/discussions` link remains.** Discussions is off, so a `/discussions` link
would 404. PR #272 (SOL-152946) repointed the three that existed at
`https://solace.community/`, and `.github/SUPPORT.md` carries the routing.
Re-check rather than trusting this paragraph. The hits it returns are prose in
this file and in `OPEN_SOURCE_STATUS.md`, not links:

```bash
grep -rn '/discussions' .github/
```

**Outstanding:** those links go to the Solace Community root rather than to a
category specific to this project, because the category does not exist yet.
Repointing them once it does is SOL-152413. Nothing is broken meanwhile.

---

## Security Settings

Navigate to: **Settings → Security → Code security and analysis**

Current state, re-read from the repository API on 2026-08-07. These settings change
outside this file, so re-read before acting on the table rather than trusting it:

```bash
gh api repos/OWNER/REPO --jq '.security_and_analysis'
```

| Setting | Now | Action |
|---------|-----|--------|
| Dependabot alerts | On | None |
| Dependabot security updates | On | None |
| Secret scanning | On | None |
| Secret scanning push protection | On | None |
| Secret scanning validity checks | On | None |
| Secret scanning non-provider patterns | On | None |
| Secret scanning AI detection | On | None |
| CodeQL | Running — `Analyze (go)` passes on every PR, but **not a required check** | **Outstanding:** make it required, SOL-152858 |
| Private vulnerability reporting | Not enabled | **Outstanding:** enable it, SOL-152967 |

Both secret-scanning settings are on, so there is nothing to enable there. Confirm
rather than change: alerts find credentials already committed, push protection
rejects the next one before it lands, and losing either is a regression. Both are
free on a public repository.

Two related facts, both established by the open-source readiness review (PR #15)
rather than re-derived here: git history was searched for committed credentials
(`git log --all -S "password"` and equivalents) and came back clean, and
`.gitignore` covers `.env` and `broker-config.yaml` — the two files most likely to
carry a real broker credential on a contributor's machine. The `.gitignore` half is
cheap to re-check; the history half is worth re-running before citing it.

**CodeQL runs but does not block.** A check named `Analyze (go)` runs and passes on
every pull request, and its workflow path points at the code-scanning default
setup. Against that, the repository API reports Code Security as disabled and the
default-setup endpoint returns 403. Something is producing this check and we cannot
say from the API which configuration owns it — so a pull request that introduces a
CodeQL finding merges today. SOL-152858 covers both halves: establish where the
switch lives, then add `Analyze (go)` to the ruleset. Requiring a check whose owner
we cannot name is the wrong order, which is why it is not already in the list.

**Private vulnerability reporting is not enabled** (SOL-152967). Enabling it gives
a security researcher a route that is not a public issue, which is the point of the
feature. Verify with:

```bash
gh api repos/OWNER/REPO/private-vulnerability-reporting
```

> ⚠️ **Reports land in the repository's Security tab, not in support.**
> `.github/SECURITY.md` publishes `support@solace.com` and commits to acknowledging
> every report within **1 business day**. A private vulnerability report opened
> through GitHub never reaches that queue, so enabling the feature without an owner
> watching the tab creates a second intake carrying a published SLA that nobody is
> serving. Who owns the tab is an open decision on SOL-152967 — settle it at the
> same time as enabling, not after.

**Dependency updates split between Renovate and Dependabot.**

`.github/renovate.json` covers exactly one job: the pinned Claude Code CLI
version in the LLM e2e harness. `.github/dependabot.yml` covers scheduled
version updates for Go modules across all three `go.mod` files (root,
`test/e2e-common/broker-driver`, `test/e2e-basic-mcp/agent`) and GitHub
Actions — grouped to keep volume down: one PR per Go module for minor/patch,
one per module for major, one for GitHub Actions. Dependabot also still
handles alerts and security-only updates, as before; those run independently
of `dependabot.yml` and are already on.

- Renovate does not cover Go modules or GitHub Actions here. SOL-152586
  briefly extended it to do so instead of adding a Dependabot config, but
  Renovate cannot be enrolled for a public repository under the org's current
  system, so it was reverted under SOL-152808.
- Dependabot's `commit-message` config has no field for a commit trailer, so
  its PRs can never carry the `Signed-off-by` line the `DCO sign-off` required
  check normally needs. Rather than leave every one of its PRs permanently red,
  `.github/workflows/dco.yaml` skips that check specifically for PRs GitHub
  itself records as opened by `dependabot[bot]` — see that file and
  `.github/scripts/dco-check.sh` for why this is safe from PR-side forgery.

---

## Branch Protection

`main` is protected by two overlapping mechanisms. Both are live, and GitHub
applies whichever is more restrictive:

- **Ruleset `main-protection`**: carries the required status checks. Edit this one.
- **Legacy branch protection rule**: requires 1 approval, with an empty
  required-checks list.

The checks are on the ruleset, and that is where any future one goes. Adding a
check to the legacy rule instead creates a second list to keep in sync, which is
how they drift apart. The legacy rule now adds nothing the ruleset does not already
do more strictly — it requires 1 approval with no Code Owners requirement and no
contexts — so retiring it is safe and would remove a place for a check to be added
by mistake.

Navigate to: **Settings → Rules → Rulesets → `main-protection`**

**Pull request rule.** ✅ = should be on.

| Setting | Target | Live now |
|---------|--------|----------|
| Required approvals | ✅ 1 | Set |
| Dismiss stale approvals on new commits | ✅ On | Set |
| Require review from Code Owners | ✅ On | Set |
| Require conversation resolution before merging | ✅ On | Set |
| Require signed commits | ⬜ Optional (DCO is sufficient) | Not set |

`.github/CODEOWNERS` maps `*` to `@SolaceProducts/dax-developers`, and Code Owners
review is now enabled (SOL-152961), so every contribution — external ones
included — needs a dax-developers review. That is the one enforced human gate on
`main`; see the Rulesets section for why Copilot review is not a second one.

**Other rules:**

| Setting | Target | Live now |
|---------|--------|----------|
| Restrict deletions | ✅ On | Set |
| Block force pushes | ✅ On | Set |
| Bypass list empty, so rules apply to admins too | ✅ Empty | Empty |

### Required status checks

**Require branches to be up to date before merging** is on, and **the context list
is now applied** — corrected on 2026-08-07 under SOL-152412, replacing the old
`lint` / `build` / `FOSSA Scan` list. Thirteen contexts are registered. This table
is the single authoritative list; the Rulesets section deliberately does not
repeat it.

Read the live list rather than trusting the table:

```bash
gh api repos/OWNER/REPO/rulesets/13942241 \
  --jq '.rules[] | select(.type=="required_status_checks")
        | .parameters.required_status_checks[] | "\(.context)\t\(.integration_id // "UNPINNED")"'
```

| Check | Source | In ruleset | Gates on |
|-------|--------|-----------|----------|
| `lint` | `build-and-test.yml` job `lint` | Yes, pinned 15368 | golangci-lint — 8 linters incl. `gosec`, `staticcheck`, `bodyclose`, `noctx` (`.golangci.yml`) |
| `build` | `build-and-test.yml` job `build` | Yes, pinned 15368 | build, vet, race tests |
| `e2e-basic-mcp` | `build-and-test.yml` | Yes, pinned 15368 | E2E suite |
| `e2e-oauth` | `build-and-test.yml` | Yes, pinned 15368 | E2E suite |
| `e2e-monitoring` | `build-and-test.yml` | Yes, pinned 15368 | E2E suite (known flaky fixture; rerun the job before investigating) |
| `e2e-management` | `build-and-test.yml` | Yes, pinned 15368 | E2E suite |
| `e2e-action` | `build-and-test.yml` | Yes, pinned 15368 | E2E suite |
| `Guardian scan gate` | `guardian-scan.yaml` job `gate` | Yes, **unpinned** | The Guardian scan verdict, or an accounted-for reason there is none (fork PR). Replaces `SCA gate`; see the warnings below |
| `Third-party licenses current` | `ci-pr.yaml` job `licenses` | Yes, pinned 15368 | `THIRD_PARTY_LICENSES.md` still matching `go list -deps ./cmd/server`. Needs no secret, so it reports on fork pull requests too |
| `CHANGELOG updated` | `ci-pr.yaml` job `changelog` | Yes, pinned 15368 | Advisory today; see note below |
| `DCO sign-off` | `dco.yaml` job `dco` | Yes, **unpinned** | a sign-off on every commit the PR adds, except a PR GitHub records as opened by `dependabot[bot]` (SOL-152808) |
| `DCO check self-test` | `ci-pr.yaml` job `dco_selftest` | Yes, **unpinned** | the gate's own logic still working |
| `Licence headers present` | `ci-pr.yaml` job `license_headers` | Yes, pinned 15368 | Every `.go` file outside `vendor/` opening with the Apache-2.0 header. Needs no secret and no Go toolchain, so it reports on fork pull requests too |

⚠️ **`Licence headers present` is spelled the British way.** `Licence`, not
`License`. The job landed on `main` with PR #261 (SOL-152896) and was registered
the same day, verified green on that PR's head (`10f6a00`) under app id 15368.
Worth knowing if it ever has to be re-added: `ci-pr.yaml` triggers on
`pull_request` only, so this check never appears on a `main` commit and GitHub's
context picker — which suggests names seen recently on the default branch — may
not list it. Both DCO contexts have the same property. Type the name in by hand if
so, and check the spelling.

**A newly required check reads pending on pull requests opened before it existed,
and that is not a misconfiguration.** `ci-pr.yaml` runs from the pull request's own
ref, so a branch that predates the job simply does not produce the check —
`Licence headers present` reads "Expected — waiting for status to be reported" on
all three pull requests open when it was registered (#278, #263, #245), none of
which carries the `license_headers` job. Rebasing onto `main` resolves it, and
`strict_required_status_checks_policy: true` already requires those branches to be
up to date before merging, so no extra work is imposed. Expect this every time a
context is added; do not read it as the trap described below. The distinction is
whether the check *can* report from a rebased branch — `Guardian scan` never can,
because it is not a job at all.

⚠️ **Three of the thirteen contexts are registered unpinned** — `Guardian scan gate`,
`DCO sign-off`, and `DCO check self-test` carry no `integration_id`, so the
ruleset requires *a context with that name from any source* rather than that
context from GitHub Actions. Any actor holding `statuses: write` on the repository
could satisfy one by posting a commit status with the matching name. Low severity
— that permission is not widely held, and the pinned ten cannot be spoofed this
way — but the three unpinned ones are the supply-chain gate and both DCO controls,
which is the worst subset to leave open. Fix by re-adding each context from the
picker (which pins it) rather than typing the name, and confirm with the `gh api`
command above that `UNPINNED` no longer appears.

⚠️ **A scan outage blocks every merge.** The `gate` job fails closed by design, and
it is required, so "scanning is broken" and "no pull request can merge" are the
same state. That is the intended trade — the list it replaced was
`lint`/`build`/`FOSSA Scan`, under which a scan outage blocked nothing — but
whoever is on point should know that a red `Guardian scan gate` may be the tooling
rather than the code. The SHA pin buys less than it looks like: `guardian-scan.yaml`
pins the `SolaceDev/solace-public-workflows` actions, but those actions run
container images whose nested logic can change without a line changing here.
Repinning is not a substitute for watching a run.

On the FOSSA licensing findings that were expected to block `main` — the
`marshmallow` CC-BY-SA-4.0 policy denial and two `hashicorp` MPL-2.0 detections on
the `SolaceProducts_solace-broker-mcp` project — that block did not materialise on
the runs observed: `FOSSA licensing` is green in `BLOCK` mode on the most recent
push to `main` (2026-08-07 21:24 UTC). Three `main` runs did fail earlier the same
day, two of them at `Guardian gate` alone. Check the current run rather than either
prediction. A red `gate` for a real finding is the gate working.

⚠️ **`Guardian scan` and `Guardian gate` are traps. Neither belongs in the list.**
Both were added by mistake on 2026-08-07 alongside the correct contexts and
removed within minutes. GitHub's picker offers both, and both look like the right
answer.

- **`Guardian scan` echoes the workflow name, and workflows are not checks.**
  `guardian-scan.yaml` line 1 reads `name: Guardian Scan`. Workflow names never
  become check contexts — only job names do — so no check run named `Guardian scan`
  or `Guardian Scan` has ever existed on this repository, in any casing.
  A required context that never reports sits at "Expected — waiting for status to
  be reported" forever, which is a **hard block on every pull request**. This is
  what it did: no merge to `main` was possible until the context was removed.
- **`Guardian gate` is a real job that never runs on a pull request.**
  `guardian-scan.yaml` job `guardian-gate` (`name: Guardian gate`) carries
  `if: ${{ !cancelled() && !fromJSON(needs.setup.outputs.is_fork) && fromJSON(needs.setup.outputs.is_trunk) }}`.
  The `is_trunk` term means it runs on push to `main` and is **always skipped on a
  pull request** — observed skipped on PR #261's head. GitHub counts a skipped
  check as passing, so requiring it produces a green tick that gates nothing. That
  is precisely the `FOSSA Scan` trap this ticket existed to remove. The name is
  doubly confusable: `release-readiness-check.yaml` also names a job
  `Guardian gate`, on the tag path.

The two failure modes are opposite and both are bad: a context that never reports
blocks everything loudly, and a context that always skips blocks nothing quietly.
Only the second one is easy to miss. `Guardian scan gate` — job `gate`, `if:
always()` — is the one that reports on every pull request and is the one to
require.

⚠️ **Require `Guardian scan gate`, and nothing FOSSA-shaped.** The old
`FOSSA Scan` and `FOSSA Scan / SCA Scan` contexts no longer exist — the
Vault-backed FOSSA jobs were removed from `ci-pr.yaml`, `build-and-test.yml`, and
`fossa-scan.yaml` (deleted). Scanning now runs in `guardian-scan.yaml`, which
triggers on `pull_request` and `push` directly rather than being called, so most
of its jobs surface under their plain `name:` values.

- **`Guardian scan gate` is the one to require.** `guardian-scan.yaml` job `gate`
  always reports. It passes on a scan success, fails on a scan failure, and passes
  a fork PR's skip with a logged reason — re-deriving the fork condition itself and
  **failing** a skip it cannot account for, so narrowing the scan jobs' `if` later
  cannot quietly switch the gate off on same-repo pull requests. Same design as the
  `SCA gate` it replaced.
- **Do not require the individual scan jobs, and do not require the workflow.**
  The scan jobs — `Build image / build`, `FOSSA licensing`, `FOSSA vulnerability`,
  `Prisma scan`, and `Guardian gate` — all skip on a fork PR (no Environment
  secrets), and `Guardian gate` skips on *every* pull request. A skipped plain job
  counts as passing, and only `gate` fails closed on an *unexplained* same-repo
  skip. `Guardian scan` is not a check at all — it is the workflow's `name:`. See
  the trap warning above; both wrong names were briefly live and both had to be
  removed.

The naming rule still holds: a reusable-workflow caller that **runs** surfaces as
`<caller job name> / <inner job name>`; a caller that is **skipped** produces only
the bare caller name. `guardian-scan.yaml` is not itself called, so its ordinary
jobs are plain names — but its `build` job *is* a caller, which is why that one
reports as `Build image / build`. The rule is about the job, not the workflow.

⚠️ **Both DCO rows are required, and dropping either removes a control.** DCO
stands in for a contributor licence agreement, so the repository is not covered
until both are registered — an unrequired check enforces nothing, the same trap the
old `FOSSA Scan` context was (a required-looking check that only ever reported
skipped).

- `DCO sign-off` *is* the control. Dropping the row removes it.
- `DCO check self-test` blocks for a different reason: the gate runs the *base
  ref's* copy of `dco-check.sh`, so a PR that breaks the script shows the
  self-test red and the gate green. Unless the self-test blocks, that PR merges
  and the gate stays broken for everything after it. It guards regressions on
  trusted PRs only, since a fork can rewrite the test in its own PR — which is
  exactly why the gate itself does not live there.

Both strings are the jobs' `name:` values verbatim. They are plain jobs, not
reusable-workflow calls, so each produces exactly one context with no ` / `
suffix. Confirmed against a live run: `dco.yaml` is on `main` and both contexts
report on same-repo pull requests. Verified on PR #232 (`29fe3ab`) with

```bash
gh api repos/OWNER/REPO/commits/<head-sha>/check-runs --jq '.check_runs[].name'
```

which returned `DCO sign-off` and `DCO check self-test` alongside the other nine,
and returned bare `FOSSA Scan` as `skipped` — the trap described above, observed
rather than inferred.

Both are registered as of 2026-08-07. `dco.yaml` is on `main`, so the deadlock that
would have followed from registering `DCO sign-off` while the workflow was still
absent (`pull_request_target` runs the base ref's workflows, so the check could not
report) never applied.

If *Require approval for all outside collaborators* is on (Settings → Actions →
General), fork PR runs wait for a maintainer and their checks read **pending**
rather than failing; `pull_request_target` is not exempt. Pending fails closed, so
nothing slips through. The failure mode to avoid is reading that as a misconfigured
list and dropping a DCO row. Approve the workflow run instead.

Three more notes on the list:

- `CHANGELOG updated` cannot fail today. `ci-pr.yaml` sets
  `CHANGELOG_GATE_MODE: advisory`, so the script warns and exits 0. Requiring it
  makes the check's presence a merge precondition now, so flipping the mode to
  `blocking` later needs no ruleset change.
- A green `Guardian scan gate` on a pull request does **not** mean the pull request
  is free of licensing conflicts or critical/high vulnerabilities. See
  [What the supply-chain gate actually guarantees](#what-the-supply-chain-gate-actually-guarantees)
  immediately below — read it before answering that question for anyone.
- `Analyze (go)` (CodeQL) passes on every PR and is **not** in the list above, so a
  CodeQL finding does not block a merge. Adding it is outstanding under SOL-152858,
  behind the configuration question in the Security Settings section.

### What the supply-chain gate actually guarantees

The honest summary: **detection on `main`, not prevention at the pull request.**
The facts below are read from `guardian-scan.yaml`; they are stated here in one
place because assembling them from three sections has already produced a wrong
answer in a go/no-go review.

**What is guaranteed.** Every push to `main` and every release tag is scanned in
enforcing mode. `setup` sets `enforce=true` only when `is_trunk` is true, which
makes `fossa_mode=BLOCK` for the licensing guard (`block_on: policy_conflict`) and
passes `fail-on-blocked: true` to the Guardian vulnerability gate
(`block_on: critical,high`). A licensing conflict or a critical/high vulnerability
therefore turns `main` red. The release tag runs the same workflow through
`release-readiness-check.yaml`, with one documented escape hatch: a repository
admin can set `fail_on_blocked: false` to ship past a Guardian block, and the
authorising step logs who did. On a same-repo pull request the gate still fails if
a scan job itself fails — an outage or a broken scan blocks the merge — and
`Third-party licenses current` independently blocks a pull request whose dependency
set has drifted from `THIRD_PARTY_LICENSES.md`, on fork pull requests too.

**What is not guaranteed.** A pull request carrying a licensing conflict or a
critical/high vulnerability **can merge**.

- On a **same-repo** pull request, `is_trunk` is false, so the licensing guard runs
  in REPORT and the vulnerability guard is REPORT with no Guardian gate behind it
  (`guardian-gate` requires `is_trunk`). Both run in diff mode, so they see only
  what is new relative to the base branch — a green run is not a clean full scan.
  Findings appear as a pull request comment; nothing exits non-zero.
- On a **fork** pull request, the scan jobs are skipped entirely for want of
  Environment secrets, and `gate` exits 0 with a logged notice. There is no verdict
  at all.

So the first enforcing scan of a change happens *after* it has merged. That is a
deliberate trade — a pre-merge enforcing scan would need Environment secrets on a
fork-triggered run — but it should be described as what it is.

**What stands in.** Two human controls, both listed in the fork section below and
neither automatic: read `go.mod`/`go.sum` on any pull request that touches them,
and read the `.github/` diff on any fork pull request. A red scan on `main` also
needs a named owner; nothing routes it today. `govulncheck` or `osv-scanner` needs
no secret and would restore real pre-merge prevention for the vulnerability half —
see "Worth closing properly" below.

**How a fork pull request gets a verdict.** SOL-152411 fixed the two problems that
made this impossible. What it changed, and what it deliberately did not:

1. **The seven `build-and-test.yml` checks now report.** That workflow used to
   trigger on `push` only, so a fork contributor's push fired in their fork and
   `lint`, `build`, and the five `e2e-*` checks never reported on this
   repository's commit — required contexts pending forever. It now also triggers
   on `pull_request`. None of those jobs reads a secret: the e2e suites pull
   public images (`solace/solace-pubsub-standard`, `quay.io/keycloak/keycloak`),
   take their broker credentials from committed `.env` files, and generate their
   own self-signed TLS certs with `openssl`. A read-only `GITHUB_TOKEN` is enough.
2. **Scanning no longer goes red for a missing secret.** GitHub withholds
   Environment secrets from a fork-triggered `pull_request` run, so
   `guardian-scan.yaml`'s scan jobs are skipped for fork pull requests, and the
   always-reporting `Guardian scan gate` job is the required context instead.
   (Before the move off Vault this was `fossa_scan` finding an empty `VAULT_URL`
   and exiting 1 on every external contribution.)
3. **`pull_request_target` was not introduced**, and neither was a `workflow_run`
   follow-up. Both would run untrusted code with an elevated token to buy a
   pre-merge scan on a path that already requires a maintainer to approve the
   workflow run and a code owner to approve the pull request.

**The residual gap — it needs an owner.** A fork pull request gets no pre-merge
Guardian/SCA verdict: the scan jobs need Environment secrets, forks don't get them,
so a fork pull request merges unscanned and is only caught by the scan on push to
`main` — detection after merge, not prevention. Fork pull requests are the normal
contribution path for this repository, so this is the common case rather than an
edge one. Note that "require approval for outside collaborators" (GitHub Actions
Permissions section) protects the *secrets*, not the *verdict* — an approved fork
run still can't scan. **This is a decision for a repo owner to make deliberately,
not a default to inherit.** Until it's closed, two human controls stand in, and
both need someone to actually do them:

- **Read `go.mod` and `go.sum`** in any fork pull request that touches them.
- **Read the `.github/` diff too.** Moving to `pull_request` means a fork pull
  request supplies the workflow definitions that run for it. It cannot reach a
  secret and its token is read-only, so this is not a credential-theft path, but a
  pull request can still weaken its own checks — including `Guardian scan gate`,
  which runs from the pull request's own ref. `DCO sign-off` is the exception:
  `dco.yaml` runs on `pull_request_target` so a pull request cannot edit the copy
  that judges it.
- **A red FOSSA on `main` needs an owner.** Nothing routes it today: the failure
  lands on an already-merged commit, and no alert or assignee follows from it.
  Whoever merges a dependency-touching fork pull request should watch the next
  `main` run themselves.

**Worth closing properly.** The vulnerability half of this gap does not actually
need a credential. `govulncheck` (or `osv-scanner` over `go.mod`) needs no secret,
no Vault, and no elevated token, so it would run fine as an eighth job in
`build-and-test.yml` on a fork pull request — restoring *prevention* for
critical/high vulnerabilities instead of leaving both halves to a human. Licensing
is the harder half; `go-licenses` covers some of it. This was left out of
SOL-152411 to keep that change to the trigger and gate problem, not because it is
unavailable. It is the obvious follow-up.

⚠️ **None of the fork behavior above has been observed on a real external
contribution** — no fork pull request has been opened against this repository yet,
so it is derived from the workflow definitions rather than watched. `CHANGELOG
updated` should be fine: `ci-pr.yaml` gives that job only `contents: read` and it
reads no secrets. `Analyze (go)` appears to come from CodeQL default setup, which
the Security Settings section asks you to confirm. Copilot review is inconsistent
even on same-repo pull requests, so do not count on it. Check `Guardian scan gate`,
`CHANGELOG updated`, `Analyze (go)`, and the `lint` job's annotation behavior on a
read-only token against the first real fork pull request, and correct this document
from what you see.

**A third problem, and this one we create deliberately.** The GitHub Actions
Permissions section below sets approval to be required for all outside
collaborators. That holds the entire workflow run, not just one job, until a
maintainer approves it.

The consequence, in order: an external contributor opens their first pull request,
and **no checks run at all**. Not `Guardian scan gate`, not `CHANGELOG updated`, not
the seven from `build-and-test.yml`. Every required context sits pending and the pull request
reads as stuck rather than as rejected or passing. This is independent of the
SOL-152411 fix: that made the checks *capable* of reporting on a fork pull request,
and this setting decides *when* they are allowed to start.

That is expected, not broken. Maintainers need to know it, or a community
contribution gets triaged as a CI outage. Two things follow:

- Watch the Actions tab for runs awaiting approval, not just the pull request page.
- Tell the contributor you are waiting on an approval click, rather than leaving
  them looking at a grey check.

This also constrains the required-check list. Every context you require is a
context that reports only after a human approves the run, so requiring more of
them lengthens the stall rather than tightening the gate.

Copying a community branch into this repository to test it is no longer necessary,
and is now the worse option: a same-repo branch is a trusted context, so doing it
runs the contributor's code with this repository's secrets. Approve the fork run
instead.

---

## GitHub Actions Permissions

Navigate to: **Settings → Actions → General → Workflow permissions**

**Workflow permissions:**
- ✅ **Read repository contents permission: set.** The default is now read-only
  (SOL-152959). The old "Read and write" default was never actually needed —
  every job that writes (`release.yml`'s `release`/`build-docker`/`build-binaries`
  jobs) already declares its own explicit `contents: write` /
  `packages: write` / etc., which overrides the repo default regardless of what
  it is. `make check` passes on this change; the release path itself is only
  exercised by an actual tag push, which has not happened since — watch the
  next one.
- ✅ **"Allow GitHub Actions to create and approve pull requests" turned off**
  (SOL-152958). With a write `GITHUB_TOKEN` and a `main` ruleset that needs
  exactly one approval and has an empty bypass list, a workflow could otherwise
  approve a pull request and satisfy the only human gate on `main`.

  The exposure was not a fork contributor. On a public repo a fork PR's
  `GITHUB_TOKEN` is read-only. It was a workflow running on a branch *inside* this
  repository, and this repository allows all actions with no SHA pinning required
  (see "Fork pull request workflows" below), so a compromised third-party action
  would have inherited that ability. Nothing here needed the setting: Renovate opens
  PRs with its own GitHub App installation token, and no workflow in this
  repository opens or approves PRs.

**Alternative (more restrictive):**
- ⚪ Read repository contents and packages permissions
- Manually grant write access to release workflow via repo secrets

### Fork pull request workflows

Navigate to: **Settings → Actions → General → Fork pull request workflows from
outside collaborators**

GitHub offers three approval postures, and the default is the middle one:

1. Require approval for first-time contributors who are new to GitHub
2. **Require approval for first-time contributors** (the default)
3. Require approval for all outside collaborators

**The intended posture is option 3.** GitHub defines the default as "only users who
have never had a commit or pull request merged into this repository will require
approval", so a contributor is exempt permanently once any one of their
contributions merges. This repository allows all actions and does not require SHA
pinning (`allowed_actions: all`, `sha_pinning_required: false`), so until that
tightens, a human should look at every external workflow run before it executes.

⚠️ **Outstanding, and unverifiable by tooling** (SOL-152960). **No GitHub REST API
exposes this setting** — `GET /repos/OWNER/REPO/actions/permissions*` does not
surface it, and the org-level endpoint needs an `admin:org` scope. SOL-152960 is
closed, but a closed ticket is not evidence of a live setting, and treating it as
evidence would be the same class of error as the `FOSSA Scan` context. Someone with
org-admin access has to open the page, read the radio button, and say what it says.
Until then, treat option 3 as intended rather than confirmed.

This posture has a cost worth knowing: until a maintainer approves the run, **no
checks report at all** on an external contributor's pull request, so it shows
pending required contexts rather than a verdict. That is expected behavior, not a
CI failure. The fork-pull-request warning at the end of the Branch Protection
section explains what maintainers should do about it.

---

## Releases

v0.1.0 through v0.6.0 are tagged and published, v0.6.0 on 2026-07-28. The
mechanics live in `RELEASING.md`; what belongs here is the one release fact with an
admin consequence.

⚠️ **Outstanding: the latest release does not describe `main`.** `main` is **130
commits** ahead of v0.6.0 (read 2026-08-07), and `[Unreleased]` in `CHANGELOG.md`
carries a substantial Added/Changed/Fixed/Security set including two BREAKING
entries — the Go module path and the container image path both moved to
`SolaceProducts`. Anyone who lands on the latest release rather than on `main` gets
a materially different product, and an import path and image reference that no
longer work. Cut a release. Re-derive rather than trusting the count here:

```bash
git rev-list --count v0.6.0..origin/main
```

and read the `[Unreleased]` block.

---

## Repository Roles

Navigate to: **Settings → Collaborators and teams**

**Recommended team structure:**

| Role | Users/Teams | Permissions |
|------|-------------|-------------|
| **Admin** | Repository owners | Full access |
| **Maintain** | Core maintainers | Merge PRs, manage issues, configure settings |
| **Write** | Regular contributors | Push to branches, create PRs |
| **Triage** | Community moderators | Label issues, close duplicates |
| **Read** | Public | Read-only (default for public repos) |

---

## Advanced Settings

Navigate to: **Settings → General → Pull Requests**

| Setting | Target | Live now |
|---------|--------|----------|
| Allow merge commits | ✅ On | On |
| Allow squash merging | ✅ On | On |
| Allow rebase merging | ✅ On | On |
| Automatically delete head branches | ✅ On | On |
| Always suggest updating pull request branches | ⬜ No effect here | Off |

"Always suggest updating pull request branches" changes nothing on `main`, so
leave it. GitHub only restricts the "Update branch" button when the base branch
does *not* require branches to be up to date; the setting lifts that restriction.
`main-protection` sets `strict_required_status_checks_policy: true`, so the button
is already offered on every stale pull request to `main`.

Turn the setting on if you want the button on pull requests targeting other
branches. `main-protection` applies to `~DEFAULT_BRANCH` only, so any pull request
against a non-default base already lacks it today. Nobody is asking for that, so
leaving the setting off is fine.

"Update branch" needs write access to the head branch either way, so a fork
contributor never gets the button and has to rebase locally.

**Default commit messages.** Live values, which differ from the labels in the UI:

| Merge type | Title | Message |
|------------|-------|---------|
| Squash | `COMMIT_OR_PR_TITLE` | `COMMIT_MESSAGES` |
| Merge | `MERGE_MESSAGE` | `PR_TITLE` |

These are the defaults and no change is needed. They are recorded here so a future
reader can tell a deliberate setting from drift.

---

## GitHub Pages (Optional)

Navigate to: **Settings → Pages**

If you want to host documentation via GitHub Pages:
- Source: Deploy from a branch
- Branch: `gh-pages` or `main`
- Folder: `/docs` or `/ (root)`

**Not required for this project** - README.md and docs/ folder are sufficient.

---

## Rulesets

Navigate to: **Settings → Rules → Rulesets**

Rulesets are the mechanism this repository already uses. **Two** are active, read
live on 2026-08-07 (`gh api repos/OWNER/REPO/rulesets`):

| Ruleset | ID | What it does |
|---------|----|--------------|
| `main-protection` | `13942241` | PR review, required status checks, blocks force pushes and deletions. See [Branch Protection](#branch-protection). |
| `Code Quality Copilot review for default branch` | `19496217` | Requests a Copilot review on pull requests to `main`, drafts included (`review_draft_pull_requests: true`), **not** on every push (`review_on_push: false`) |

A third, `Copilot review for default branch` (`16246311`), was **deleted on
2026-08-07**. Its `deletion` and `non_fast_forward` rules duplicated
`main-protection` on the same ref, so no protection was lost — GitHub applies the
union of matching rulesets, and `main-protection` still carries both.

Copilot review is a nudge, not a gate. It is worth being explicit about this
because "Require conversation resolution before merging" is now on and looks like
it makes Copilot enforcing:

- Conversation resolution counts Copilot's review threads alongside human ones, so
  an unresolved Copilot comment does hold the merge button.
- But the pull request author can resolve threads on their own pull request, with
  no second pair of eyes. So it enforces *acknowledgement*, not *agreement*.
- Copilot review is also inconsistent even on same-repo pull requests. Do not count
  it among the enforced gates; the enforced list is the 13 required contexts and
  the Code Owners approval.

Configure `main-protection` per the Branch Protection section above. That section
is the single source for the required-check names; do not maintain a second copy
here.

---

## Community Launch Checklist

Presentation and reach, as distinct from configuration. The security and
configuration work is tracked in [Outstanding actions](#outstanding-actions) at the
top of this document.

- [ ] Verify all badges in README.md render correctly
- [ ] Check that CODE_OF_CONDUCT and CONTRIBUTING show in sidebar
- [ ] Test issue template flow (create a test issue, then close it)
- [ ] Test PR template (create a test PR, then close it)
- [ ] Verify SECURITY.md appears in Security tab
- [ ] Submit to Go package indexes (if applicable)
- [ ] Confirm the project's Solace Community category is live, then announce there
- [ ] Share on relevant channels (Twitter, LinkedIn, etc.)
- [ ] Add to MCP server registry (if one exists)

---

## Monitoring & Maintenance

**Weekly:**
- Review open issues and PRs (target <7 days for first response)
- Review Solace Community threads in the project's category
- Triage Dependabot alerts and merge any security-update PRs
- Check GitHub Insights for community health metrics

**Monthly:**
- Review this checklist for new GitHub features
- Update dependencies: `go get -u ./...`
- Check for new security advisories

**Quarterly:**
- Audit against [OpenSSF Best Practices Badge](https://bestpractices.coreinfrastructure.org/)
- Review contributor activity and thank active contributors
- Update roadmap based on community feedback

---

## Questions?

If you need help with any of these admin tasks, contact:
- support@solace.com
- Solace DevRel team
