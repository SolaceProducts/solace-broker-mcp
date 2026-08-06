# Repository Admin Setup Checklist

This document contains tasks that require **repository admin access**. These should be completed before or immediately after making the repository public.

**Admin needed**: @solace-aross or repository administrators

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

⚠️ **Before flipping public**: three links still point at `/discussions`, which
404s while Discussions is off.

Find them, rather than trusting a line number that will drift:

```bash
grep -rn '/discussions' .github/
```

At the time of writing that returns two links in `.github/CONTRIBUTING.md` and
one in `.github/ISSUE_TEMPLATE/config.yml` (the issue-template chooser). This
also mentions "issues and discussions" in prose, with no link to break:

```bash
grep -rn 'issues and discussions' .github/
```

Repointing all of them at the Solace Community category is tracked separately.
Confirm that landed, or expect broken links from day one.

---

## Security Settings

Navigate to: **Settings → Security → Code security and analysis**

Current state, read from the repository API on 2026-07-29. These settings change
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
| CodeQL | Running - `Analyze (go)` passes on every PR | Confirm on the settings page |

Both secret-scanning settings are already on, so there is nothing to enable here.
Confirm rather than change: alerts find credentials already committed, push
protection rejects the next one before it lands, and losing either is a
regression. Both stay free on public repositories.

On CodeQL: a check named `Analyze (go)` runs and passes on every PR, and its
workflow path points at the code-scanning default setup. Against that, the
repository API reports Code Security as disabled and the default-setup endpoint
returns 403. Something is producing this check and we cannot say from the API
which configuration owns it. Confirm on the settings page before relying on it,
and re-check after the visibility change.

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
  system — reverted under SOL-152808 ahead of this repo going public.
- Dependabot's `commit-message` config has no field for a commit trailer, so
  its PRs can never carry the `Signed-off-by` line the `DCO sign-off` required
  check normally needs. Rather than leave every one of its PRs permanently red,
  `.github/workflows/dco.yaml` skips that check specifically for PRs GitHub
  itself records as opened by `dependabot[bot]` — see that file and
  `.github/scripts/dco-check.sh` for why this is safe from PR-side forgery.

---

## Guardian Scanning Setup

Supply-chain (FOSSA) and container (Prisma Cloud) scanning runs in
`.github/workflows/guardian-scan.yaml` and reports to
[Guardian](https://guardian.solacedev.ca). Because this repository is public, it
uses **native GitHub secrets in a GitHub Environment**, not Vault — a job that
reaches Vault by OIDC cannot run here, and internal hostnames must not appear in a
public repo.

### Create the `guardian` Environment

Navigate to: **Settings → Environments → New environment**, name it `guardian`.

- **Do not add required reviewers.** An Environment that requires approval pauses
  every pull request and every push to `main` waiting for a click. This is a
  secret scope, not a deployment gate.
- **Do not restrict deployment branches.** The scan runs on pull-request branches
  and on `main`; a branch restriction would deny those runs the secrets. Leave
  deployment branches set to `All branches`.

### Add these secrets to the `guardian` Environment

| Secret | What it is |
|--------|-----------|
| `FOSSA_API_KEY` | FOSSA API token (was Vault `FOSSA_FULL_API_TOKEN`) |
| `GUARDIAN_URL` | Guardian API base URL — held as a **secret**, not a variable |
| `GUARDIAN_API_TOKEN` | Guardian API bearer token |
| `PRISMACLOUD_ACCESS_KEY_ID` | Prisma Cloud access key id |
| `PRISMACLOUD_SECRET_KEY` | Prisma Cloud secret key |
| `PRISMACLOUD_CONSOLE_URL` | Prisma Cloud Console URL |

A fork pull request gets none of these — GitHub withholds Environment secrets from
a fork-triggered run — so the scan jobs (build, FOSSA licensing/vulnerability,
Prisma, Guardian gate) skip on a fork PR and the always-reporting `Guardian scan
gate` job accounts for the skip. The old
`VAULT_URL` repository secret is no longer read by any scanning workflow; it can
be removed once `transition_on_merge.yaml` (Jira transitions, still on Vault) is
also migrated.

### Product registration

`solace-broker-mcp` is registered in Guardian under squad `broker`
(`POST /api/v1/products`), so the squad-level gate thresholds apply. Registration
must exist before the first push to `main` runs the Guardian DB sync and gate.

---

## Branch Protection

`main` is protected by two overlapping mechanisms. Both are live, and GitHub
applies whichever is more restrictive:

- **Ruleset `main-protection`**: carries the required status checks. Edit this one.
- **Legacy branch protection rule**: requires 1 approval, with an empty
  required-checks list.

Put the checks on the ruleset. Adding them to the legacy rule instead leaves the
ruleset's stale list in force, and two lists to keep in sync is how they drift
apart. Consider retiring the legacy rule once the ruleset covers everything it does.

Navigate to: **Settings → Rules → Rulesets → `main-protection`**

**Pull request rule.** ✅ = should be on.

| Setting | Target | Live now |
|---------|--------|----------|
| Required approvals | ✅ 1 | Set |
| Dismiss stale approvals on new commits | ✅ On | Set |
| Require review from Code Owners | ✅ On | **Not set** |
| Require conversation resolution before merging | ✅ On | **Not set** |
| Require signed commits | ⬜ Optional (DCO is sufficient) | Not set |

`.github/CODEOWNERS` exists (`* @SolaceProducts/dax-developers`), so Code Owners review
is available. Enabling it means every external contribution needs a dax-developers
review.

**Other rules:**

| Setting | Target | Live now |
|---------|--------|----------|
| Restrict deletions | ✅ On | Set |
| Block force pushes | ✅ On | Set |
| Bypass list empty, so rules apply to admins too | ✅ Empty | Empty |

### Required status checks

**Require branches to be up to date before merging** is already on. The context
list is not: it currently holds `lint`, `build`, and `FOSSA Scan`, the last of
which enforces nothing (see the warning below). Replace it with the following.
These names are what GitHub actually reports, confirmed against the check runs on
PRs #213, #216, and #217 — except `Guardian scan gate` (new, replaces `SCA gate`)
and `Third-party licenses current` (SOL-152414), which are new. Confirm those two
against a real pull request before you rely on them:

| Check | Source | Gates on |
|-------|--------|----------|
| `lint` | `build-and-test.yml` job `lint` | golangci-lint |
| `build` | `build-and-test.yml` job `build` | build, vet, race tests |
| `e2e-basic-mcp` | `build-and-test.yml` | E2E suite |
| `e2e-oauth` | `build-and-test.yml` | E2E suite |
| `e2e-monitoring` | `build-and-test.yml` | E2E suite (known flaky fixture; rerun the job before investigating) |
| `e2e-management` | `build-and-test.yml` | E2E suite |
| `e2e-action` | `build-and-test.yml` | E2E suite |
| `Guardian scan gate` | `guardian-scan.yaml` job `gate` | The Guardian scan verdict, or an accounted-for reason there is none (fork PR). Replaces `SCA gate`; see the warning below |
| `Third-party licenses current` | `ci-pr.yaml` job `licenses` | `THIRD_PARTY_LICENSES.md` still matching `go list -deps ./cmd/server`. Needs no secret, so it reports on fork pull requests too |
| `CHANGELOG updated` | `ci-pr.yaml` job `changelog` | Advisory today; see note below |
| `DCO sign-off` | `dco.yaml` job `dco` | a sign-off on every commit the PR adds, except a PR GitHub records as opened by `dependabot[bot]` (SOL-152808) |
| `DCO check self-test` | `ci-pr.yaml` job `dco_selftest` | the gate's own logic still working |

⚠️ **Require `Guardian scan gate`, and nothing FOSSA-shaped.** The old
`FOSSA Scan` and `FOSSA Scan / SCA Scan` contexts no longer exist — the
Vault-backed FOSSA jobs were removed from `ci-pr.yaml`, `build-and-test.yml`, and
`fossa-scan.yaml` (deleted). Scanning now runs in `guardian-scan.yaml`, which
triggers on `pull_request` and `push` directly, so its jobs surface under their
plain names with no `caller / inner` suffix.

- **`Guardian scan gate` is the one to require.** `guardian-scan.yaml` job `gate`
  always reports. It passes on a scan success, fails on a scan failure, and passes
  a fork PR's skip with a logged reason — re-deriving the fork condition itself and
  **failing** a skip it cannot account for, so narrowing the scan jobs' `if` later
  cannot quietly switch the gate off on same-repo pull requests. Same design as the
  `SCA gate` it replaced.
- **Do not require the individual scan jobs.** `build`, `fossa-license`,
  `fossa-vuln`, `prisma`, and `guardian-gate` all skip on a fork PR (no Environment
  secrets); a skipped plain job counts as passing, and only `gate` fails closed on
  an *unexplained* same-repo skip. Require `Guardian scan gate`, not the scans.

The naming rule still holds: a reusable-workflow caller that **runs** surfaces as
`<caller job name> / <inner job name>`; a caller that is **skipped** produces only
the bare caller name. `guardian-scan.yaml` sidesteps it by triggering directly
rather than being called, so its contexts are plain job names.

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

Both are ready to register now. The workflow is on `main`, so the deadlock that
would follow from registering `DCO sign-off` while `dco.yaml` was still absent
(`pull_request_target` runs the base ref's workflows, so the check could not
report) no longer applies.

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
- On a **same-repo** pull request, FOSSA runs in diff mode (only issues new
  relative to the base branch) and in REPORT, not BLOCK. So the PR surfaces new
  findings without blocking on them; the hard gate is on push to `main` and at the
  release tag. Do not read a green PR as a clean full scan. On a fork pull request
  the scan does not run at all — see the fork section below.
- `Analyze (go)` (CodeQL) passes on every PR. Add it if you want code scanning to
  block merges, after settling the configuration question in the Security Settings
  section. It is not in the list above because that is a policy call, not a
  correctness fix.

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
   Environment secrets from a fork-triggered `pull_request` run, so the
   `guardian-scan.yaml` `Guardian scan` job is skipped for fork pull requests, and
   the always-reporting `Guardian scan gate` job is the required context instead.
   (Before the move off Vault this was `fossa_scan` finding an empty `VAULT_URL`
   and exiting 1 on every external contribution.)
3. **`pull_request_target` was not introduced**, and neither was a `workflow_run`
   follow-up. Both would run untrusted code with an elevated token to buy a
   pre-merge scan on a path that already requires a maintainer to approve the
   workflow run and a code owner to approve the pull request.

**The residual gap — get an owner before the repo flips public.** A fork pull
request gets no pre-merge Guardian/SCA verdict: the scan jobs need Environment
secrets, forks don't get them, so each fork PR merges unscanned and is only caught
by the scan on push to `main` — detection after merge, not prevention. Today
almost every PR is same-repo so the carve-out rarely fires; once the repo is
public, fork PRs become the normal contribution path and this becomes the common
case, not the edge one. Note that "require approval for outside collaborators"
(GitHub Actions Permissions section) protects the *secrets*, not the *verdict* —
an approved fork run still can't scan. **This is a decision for a repo owner to
make deliberately before going public, not a default to inherit.** Until it's
closed, two human controls stand in, and both need someone to actually do them:

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

Also worth knowing: on a same-repo pull request FOSSA runs in diff mode and in
REPORT (not BLOCK), so a green `Guardian scan gate` there is not a clean full scan
— the hard gate is on push to `main` and at the release tag.

Nobody has ever opened a fork pull request against this repository, so none of the
above is observed on a real external contribution. `CHANGELOG updated` should be
fine: `ci-pr.yaml` gives that job only `contents: read` and it reads no secrets.
`Analyze (go)` appears to come from CodeQL default setup, which the Security
Settings section asks you to confirm. Copilot review is inconsistent even on
same-repo pull requests, so do not count on it. Verify `Guardian scan gate`,
`CHANGELOG updated`, `Analyze (go)`, and the `lint` job's annotation behavior on a
read-only token against a real fork pull request before treating any of them as a
gate.

**A third problem, and this one we create deliberately.** The GitHub Actions
Permissions section below tells you to require approval for all outside
collaborators. That holds the entire workflow run, not just one job, until a
maintainer approves it.

The consequence, in order: an external contributor opens their first pull request,
and **no checks run at all**. Not `Guardian scan gate`, not `CHANGELOG updated`, not
the seven from `build-and-test.yml`. Every required context sits pending and the pull request
reads as stuck rather than as rejected or passing. This is independent of the
SOL-152411 fix: that made the checks *capable* of reporting on a fork pull request,
and this setting decides *when* they are allowed to start.

That is expected, not broken. Maintainers need to know it, or the first community
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
- ⚪ Read and write permissions (needed for release workflow to create releases).
  Currently set.
- 🚨 **Allow GitHub Actions to create and approve pull requests: turn this OFF.**
  It is currently ON. With a write `GITHUB_TOKEN` and a `main` ruleset that
  needs exactly one approval and has an empty bypass list, a workflow can approve
  a pull request and satisfy the only human gate on `main`.

  The exposure is not a fork contributor. On a public repo a fork PR's
  `GITHUB_TOKEN` is read-only. It is a workflow running on a branch *inside* this
  repository, and this repository allows all actions with no SHA pinning required
  (see "Fork pull request workflows" below), so a compromised third-party action
  inherits that ability. Nothing here needs the setting: Renovate opens PRs with
  its own GitHub App installation token, and no workflow in this repository opens
  or approves PRs.

**Alternative (more restrictive):**
- ⚪ Read repository contents and packages permissions
- Manually grant write access to release workflow via repo secrets

**Fork pull request workflows**

Navigate to: **Settings → Actions → General → Fork pull request workflows from
outside collaborators**

Pick the approval posture before the repo goes public. GitHub offers three
options, and the default on a public repo is the middle one:

1. Require approval for first-time contributors who are new to GitHub
2. **Require approval for first-time contributors** (the default)
3. Require approval for all outside collaborators

Choose option 3. GitHub defines the default as "only users who have never had a
commit or pull request merged into this repository will require approval", so a
contributor is exempt permanently once any one of their contributions merges. This
repository allows all actions and does not require SHA pinning
(`allowed_actions: all`, `sha_pinning_required: false`), so until that tightens, a
human should look at every external workflow run before it executes.

This has a cost worth knowing: until a maintainer approves the run, **no checks
report at all** on an external contributor's pull request, so it shows pending
required contexts rather than a verdict. That is expected behavior, not a CI
failure. The fork-pull-request warning at the end of the Branch Protection section
explains what maintainers should do about it.

---

## Visibility Settings

Navigate to: **Settings → General → Danger Zone**

The repository is currently **internal**, not private.

**BEFORE making public.** ✅ = confirmed done. ⬜ = still open at the time of
writing; re-check, do not assume.

- ✅ All CRITICAL items resolved (LICENSE, CONTRIBUTING.md, CODE_OF_CONDUCT.md)
- ✅ All HIGH items resolved (SECURITY.md, templates, CHANGELOG)
- ✅ All secrets removed from git history (`git log --all -S "password"`)
- ✅ `.gitignore` properly configured (`.env`, `broker-config.yaml`)
- ✅ No sensitive data in issues or PRs
- ⬜ **Required status checks corrected** per the Branch Protection section above.
  The ruleset still holds the old list. The supply-chain context to register is
  `Guardian scan gate` (from `guardian-scan.yaml`) — not `FOSSA Scan` and not
  `FOSSA Scan / SCA Scan`, both removed with the Vault-backed jobs.

  > **Precondition: confirm `Guardian scan gate` is green on a real pull request
  > before requiring it.**
  >
  > The `gate` job fails closed, by design. So if the scan is broken for any
  > reason, requiring it turns "scanning is broken" into "no pull request can
  > merge". Branch protection holds zero required contexts today, which is the only
  > reason a scan outage is not already blocking everyone.
  >
  > Note that the SHA pin buys less than it looks like: `guardian-scan` pins the
  > `SolaceDev/solace-public-workflows` actions, but those actions run container
  > images and can reference nested logic that changes without a line changing
  > here. Repinning is not a substitute for watching a run.
  >
  > **Expect a licensing block on `main` until the FOSSA findings are waived.** The
  > FOSSA project `SolaceProducts_solace-broker-mcp` (new since PR #243, carrying
  > none of the old project's policy waivers) reports three policy findings:
  >
  > ```
  > ⚑ Denied by policy CC-BY-SA-4.0 on github.com/perimeterx/marshmallow@v1.1.5
  > ⚑ MPL-2.0 license detected in github.com/hashicorp/go-cleanhttp@v0.5.2
  > ⚑ MPL-2.0 license detected in github.com/hashicorp/go-retryablehttp@v0.7.8
  > ```
  >
  > `guardian-scan.yaml` runs the FOSSA licensing guard in `BLOCK` mode on `main`
  > (`block_on: policy_conflict`), and the guard maps "Denied by Policy" to
  > `policy_conflict`, so the `marshmallow` finding will block a push to `main`
  > until it is waived or resolved on the FOSSA project. Vulnerability blocking is
  > separate — it is the SLA-aware Guardian gate, not FOSSA. Settle the license
  > findings before relying on the gate, or expect `main` to go red for a
  > legitimate reason.
  >
  > The `gate` job going red through all of this is the gate working.
- ⬜ **"Allow GitHub Actions to create and approve pull requests" turned off.**
  Still on.
- ⬜ **Fork pull request workflows set to require approval for all outside
  collaborators.** The default is weaker; see the GitHub Actions Permissions
  section.
- ✅ **Secret scanning and push protection enabled.** Both on, along with validity
  checks and non-provider patterns. Confirm, do not re-enable; see Security Settings.
- ⬜ **A release cut that covers `main`.** v0.1.0 through v0.6.0 are already tagged
  and published (v0.6.0 tagged 2026-07-28), so this is not about a first release
  existing. `main` is 8 commits ahead of v0.6.0 and `[Unreleased]` in
  `CHANGELOG.md` carries entries, so v0.6.0 does not describe `main`. Cut one
  before the flip. Re-check both facts rather than trusting the counts here:
  `git rev-list --count v0.6.0..origin/main` and the `[Unreleased]` block.
- ⬜ **`/discussions` links repointed** (see the Features section)

**When ready:**
1. Click **"Change visibility"**
2. Select **"Make public"**
3. Type repository name to confirm
4. Click **"I understand, make this repository public"**

⚠️ **Warning**: This cannot be easily reversed if sensitive data exists in git history!

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

Rulesets are the mechanism this repository already uses. Three are active:

| Ruleset | What it does |
|---------|--------------|
| `main-protection` | PR review, required status checks, blocks force pushes and deletions. See [Branch Protection](#branch-protection). |
| `Copilot review for default branch` | Requests a Copilot review on PRs to `main`. **Also blocks force pushes and deletions**, so retiring it drops a second layer of that protection. |
| `Code Quality Copilot review for default branch` | Copilot review including drafts and on every push |

Configure `main-protection` per the Branch Protection section above. That section
is the single source for the required-check names; do not maintain a second copy
here.

---

## Post-Publication Checklist

After repository is made public:

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
- Andrea Ross (andrea.ross@solace.com)
- Solace DevRel team
