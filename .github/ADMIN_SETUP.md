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
| Code scanning (CodeQL) | On — **default setup**, languages `actions` and `go` | **Disable default setup**, then swap the required context — see below |

Both secret-scanning settings are already on, so there is nothing to enable here.
Confirm rather than change: alerts find credentials already committed, push
protection rejects the next one before it lands, and losing either is a
regression. Both stay free on public repositories.

⚠️ **CodeQL is mid-migration from default setup to advanced setup (SOL-153411).**
`.github/workflows/codeql.yml` and its `CodeQL gate` job are in the repository; the
two remaining steps are admin actions, and their **order is load-bearing**.

Default setup and advanced setup cannot both be active, so until step 1 happens
there are two configurations competing and the required `CodeQL` context is still
the one being produced.

```bash
# Before: the configuration this migration replaces.
gh api repos/OWNER/REPO/code-scanning/default-setup
# state: configured  languages: [actions, go]  query_suite: default
# threat_model: remote  schedule: weekly  runner_type: standard
```

1. **Wait for `codeql.yml` to be green on `main`**, then disable default setup:
   **Settings → Code security → "CodeQL analysis" row → "Switch to advanced" →
   "Disable CodeQL"**. Doing this before the workflow is observed green leaves a
   window with no scanning at all.
2. **Swap the required context** on ruleset `main-protection`: **add**
   `CodeQL gate`, confirm it reports on a live pull request, **then remove**
   `CodeQL`/`57789`. Reversed, every merge blocks on a context nobody produces.
   Capture the ruleset JSON first — that file is the rollback. Net count stays 14.

```bash
# Rollback capture, before touching anything.
gh api repos/OWNER/REPO/rulesets/13942241 > /tmp/ruleset-13942241.json

# After: verify the live list, on the day it changes.
gh api repos/OWNER/REPO/rulesets/13942241 \
  --jq '.rules[] | select(.type=="required_status_checks")
        | .parameters.required_status_checks[] | "\(.context) \(.integration_id // "")"'
```

Why bother: default setup never scanned a fork pull request, a Dependabot pull
request, or a merge queue entry. See [Static analysis](#static-analysis) for what
the replacement does and does not catch.

**Nothing to enable for dependency review.** The `Dependencies free of high
advisories` check reads the Dependency Review API, which is served by the
dependency graph — always on and free for a public repository. It does not need
Code Security enabled, and the API reporting Code Security as disabled (above) does
not affect it. Confirmed live: `dependency-graph/compare/v0.6.0...v0.7.1` returns
33 dependency changes. The cost question that parked this control was about an
internal repository and no longer applies.

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
- Dependabot's pull requests pass the required `DCO` check by bot exemption, not
  by sign-off. Its commits do carry a trailer, but one whose address matches
  neither author nor committer; `dco2` never reads it, skipping bot-authored
  commits outright. That exemption is broader than the retired workflow's — see
  "Required status checks" below.

### Static analysis

What runs, what it covers, and whether it can block a merge. Read live on
19 August 2026. The point of this table is that "what static code analysis do you
run?" can be answered without opening a workflow file.

| Tool | Covers | Config | Blocking? |
|------|--------|--------|-----------|
| golangci-lint | Go static analysis. Eight linters: `errcheck`, `staticcheck`, `gosec`, `govet`, `revive`, `bodyclose`, `ineffassign`, `noctx`. `gosec` carries the security patterns (`G706` excluded pending a gosec release) | `.golangci.yml` | **Yes** — required check `lint` |
| CodeQL | SAST over Go and GitHub Actions workflows. `default` query suite, `remote` threat model, buildless Go extraction (`build-mode: none`), weekly schedule plus every push, **every** pull request (fork and Dependabot included) and every merge queue entry | `.github/workflows/codeql.yml` (advanced setup), verdict in `.github/scripts/codeql-gate.sh` | **Yes** — required check `CodeQL gate`, within the two limits below |
| FOSSA SCA | Third-party dependency licences and known vulnerabilities | `.github/workflow-config.json`, run from `guardian-scan.yaml` | **Detection, not prevention.** `Guardian scan gate` is required, but on a pull request FOSSA runs in diff mode and REPORT; the hard gate is on push to `main` and at the release tag |
| Dependency Review | A pull request *introducing* a dependency with a known high or critical advisory | `ci-pr.yaml` job `dependency_review` | Not yet a required check — see [Required status checks](#required-status-checks) |

Two limits on "a pull request with a CodeQL finding cannot merge". Both are
deliberate; neither is a defect to be fixed later without a decision. Default setup
had a third — it analyzed no Dependabot pull request and no fork pull request, so
the check concluded `neutral` there and GitHub counts `neutral` as passing, leaving
the gate absent rather than blocking. Advanced setup closes it: on 19 August 2026,
two fork pull requests and three Dependabot pull requests against `cli/cli`
(advanced setup, the same `pull_request` trigger shape as `codeql.yml`) each carried
real verdicts from both `Analyze` jobs, so SARIF upload is not blocked by the
read-only-token downgrade that applies to those events generally.

**1. Severity.** The gate fails on new alerts at severity `error`, or security
severity `critical` or `high`. A `medium` or `low` security-severity finding leaves
the gate green. The threshold is a constant in `codeql-gate.sh`, not a workflow
input — there is no scenario where turning it off is the right call, so changing it
takes a commit a reviewer can see. The ruleset rule that would enforce a threshold
instead (`code_scanning`, "Require code scanning results") is deliberately not used:
it blocks a pull request whose analysis never arrives, and it cannot express the
new-findings-only scope below.

**2. New findings only.** An alert already open on `main` is pre-existing, so a pull
request that *attempts and fails* to fix one still passes. The gate compares alert
*numbers* between the pull request ref and `refs/heads/main`; those numbers are
repository-scoped and stable across refs, so the comparison needs no rule-name and
line-number fingerprinting. Confirmation that a fix landed comes from the post-merge
`main` analysis:

```bash
gh api repos/OWNER/REPO/code-scanning/alerts/<n> --jq '.state, .fixed_at'
```

A false positive is **dismissable**: the gate reads the alerts API, not the raw
SARIF, so dismissing an alert in the Security tab with a reason stops it blocking.
That is the reason for the API round-trip — a gate nobody can open is worse than no
gate.

**The gate fails on absence, not just on findings.** A missing analysis, an analysis
that reported an extraction error, and a failed or cancelled `Analyze` matrix each
fail `CodeQL gate` rather than letting it pass quietly. `codeql-gate.test.sh` asserts
all three offline and runs as the job's first step, before the gate it guards.

On push to `main` and on the weekly schedule the gate runs in **report** mode: it
prints findings and stays green. There is no merge left to block there, and a
finding that lands on `main` must not wedge every open pull request behind a context
nobody can turn green from a pull request.

Do **not** require `Analyze (go)` or `Analyze (actions)` in place of `CodeQL gate`.
`codeql-action/analyze` has no `fail-on-severity` input of any kind, so those jobs
report success whenever analysis *completes*, whatever it found — and their names are
generated from the language matrix, so adding a language later leaves the required
set silently covering less. `CodeQL gate` is stable across language changes and
carries the verdict; it also checks that one analysis arrived *per* expected
language, which is what makes adding a language safe.

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
| Require review from Code Owners | ✅ On | Set |
| Require conversation resolution before merging | ✅ On | Set |
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

**Require branches to be up to date before merging** is on, and the context list
is applied. Sixteen contexts are documented below; **fourteen of them are
registered** in the ruleset. The two that are not carry **Not yet registered** in
their own row — they are listed here because they run on every pull request and are
candidates for registration, not because they gate anything today. This table is the
single authoritative copy of both sets.

Read the live list rather than trusting the table:

```bash
gh api repos/OWNER/REPO/rulesets/13942241 \
  --jq '.rules[] | select(.type=="required_status_checks")
        | .parameters.required_status_checks[] | "\(.context)\t\(.integration_id // "UNPINNED")"'
```

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
| `Licence headers present` | `ci-pr.yaml` job `license_headers` | Every `.go` file outside `vendor/` opening with the Apache-2.0 header. Needs no secret and no Go toolchain, so it reports on fork pull requests too |
| `Dependencies free of high advisories` | `ci-pr.yaml` job `dependency_review` | A pull request *introducing* a dependency with a known high or critical advisory, read from the Dependency Review API. Needs no third-party secret, so it reports on fork pull requests too — the vulnerability half of the fork gap below, and the only control here that prevents rather than detects. **Not yet registered** — see below |
| `CHANGELOG updated` | `ci-pr.yaml` job `changelog` | Advisory today; see note below |
| `Commit identity routable` | `ci-pr.yaml` job `identity` | Every commit the PR adds using a routable author and committer address, so a machine hostname does not publish permanently with the history. Pinned to Actions (`15368`), not to the `dco2` App — see below |
| `workflow-lint` | `workflow-lint.yaml` job `workflow-lint` | Every file under `.github/workflows/` passing actionlint (correctness, plus shellcheck over `run:` blocks) and zizmor (workflow security, including SHA pinning via `unpinned-uses`), so workflow security is enforced by a tool rather than argued in a code comment. **Not yet registered** — see below |
| `DCO` | the CNCF `dco2` GitHub App | a `Signed-off-by` trailer on every commit the pull request adds. DCO stands in for a contributor licence agreement, so this is the control behind the repository's provenance claim |
| `CodeQL` | code-scanning default setup, via the `github-advanced-security` App | **Being replaced by `CodeQL gate` — SOL-153411.** New CodeQL alerts in the code a pull request changes. Pinned to app `57789`, **not** Actions (`15368`) — every other Actions context here is `15368`, and pinning this one there leaves it permanently pending, blocking all merges. Registered 19 August 2026. Remove *after* `CodeQL gate` is added and seen reporting; see the migration steps under [Code Security](#code-security) |
| `CodeQL gate` | `codeql.yml` job `gate` | New CodeQL alerts at or above threshold, **and** the absence of a usable analysis. Pinned to Actions (`15368`) like every other workflow context here — not to app `57789`. Reports on fork pull requests, Dependabot pull requests and `merge_group` entries, which is the whole point of the migration. **Not yet registered** — see the migration steps under [Code Security](#code-security). Scope and limits: [Static analysis](#static-analysis) |

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

⚠️ **The `DCO` row is required, and dropping it removes a control.** DCO
stands in for a contributor licence agreement, so dropping the row does not weaken
the control — it removes it, along with the repository's claim that every
contribution carries a sign-off. An unrequired check enforces nothing, the same
trap the old `FOSSA Scan` context was.

⚠️ **`Commit identity routable` and `DCO` are two controls, not one, and they pin
to different sources.** `DCO` is the sign-off gate, served by the `dco2` App and
pinned to `974774`. `Commit identity routable` rejects non-routable author and
committer addresses, comes from GitHub Actions, and is pinned to `15368`. Pinning
either to the other's id means the context never matches and nothing reports.

Both are required. The App checks sign-off and nothing else, so dropping the
identity row does not weaken that control — it removes it.

**Register a new context only once it has reported green on a real pull request**,
and only after the pull request creating it has merged. Registering early leaves
every open pull request with a context its branch cannot produce. This is the rule
that would have caught the `Guardian scan` mistake.

⚠️ **`workflow-lint` is new and not yet in the ruleset.** It gates the workflow
files themselves, with actionlint and zizmor. Register it under the rule above,
and read the context name off a real run rather than assuming it:

```bash
gh run list --workflow workflow-lint.yaml --limit 1 --json databaseId \
  --jq '.[0].databaseId' \
  | xargs -I{} gh run view {} --json jobs --jq '.jobs[].name'
```

Its job id and `name:` are deliberately identical, so the context is the bare
`workflow-lint` with no `caller / inner` suffix — but read it anyway. Assuming a
name is what produced the `FOSSA Scan` versus `FOSSA Scan / SCA Scan` gap.

One property matters when registering it: `workflow-lint.yaml` carries **no
`paths:` filter**, deliberately. A required check filtered to
`.github/workflows/**` never creates a check run on a pull request that touches
only Go code, so the context stays pending forever and the pull request cannot
merge. If someone later adds a `paths:` filter to make it cheaper, this required
context is what breaks. Condition inside the job, never in the trigger.

⚠️ **`Dependencies free of high advisories` is new and not yet in the ruleset.**
Register it under the rule above — green on a real pull request first, and only
after the pull request creating it has merged. Read the context name off a real
run rather than assuming it; the job's `name:` is the context, with no
`caller / inner` suffix, but assuming a name is what produced the `FOSSA Scan`
gap.

Until it is registered it enforces nothing, which matters more here than for the
other rows: it is the one control in this repository that *prevents* a vulnerable
dependency from merging instead of finding one afterwards. The fork section below
reads as if that prevention exists. It exists as a workflow; it becomes a gate
when the context is registered.

Three properties to know before requiring it:

- **It reports on every pull request and passes trivially on most.** The action
  reads the *diff*, not the tree: a pull request that does not touch `go.mod` has
  no dependency changes and passes. So an advisory published tomorrow against a
  dependency already in the tree does not turn every open pull request red. That
  containment is what makes it safe to require, and it is why this job needs no
  `paths:` filter and no condition — see the `workflow-lint` note above for what
  a filter would cost.
- **It blocks same-repo pull requests too, Dependabot's included.** This is a
  change in daily posture, not only a fork fix. `fossa-vuln` runs REPORT on a
  pull request and blocks only on push to `main`, so a bump to a version that is
  still vulnerable used to land and go red afterwards. It now fails before merge.
- **An advisory with no fixed version has no in-repo escape, by design.** The
  escape is an admin bypass on the ruleset, which someone has to justify in the
  open. Closing that with an `allow-ghsas` list was rejected for the same reason
  the licence allow-list was: it is a policy list with no named owner, and the
  realistic failure is a contributor adding their own GHSA to unblock themselves
  and a teammate approving the one-line change.

`DCO` is served by the CNCF `dco2` GitHub App, so its `integration_id` is `974774`,
not the `15368` an Actions-produced context carries. Pinning it to Actions would
never match. Because it is an App evaluating commits server-side rather than a
workflow the pull request supplies, a pull request cannot weaken the thing that
judges it.

That server-side evaluation is also where the App is *weaker* than what it
replaced, and the two should be weighed together.

Dependabot's pull requests pass `DCO`, but not for the reason the wording invites.
Its commits do carry a trailer — `Signed-off-by: dependabot[bot]
<support@github.com>` on `20ddc122` in PR #289, where `DCO` reported `success`.
That address matches neither the author
(`49699333+dependabot[bot]@users.noreply.github.com`) nor the committer
(`noreply@github.com`), and `dco2` requires a match. It never gets that far:
`should_skip_commit` short-circuits on `author.is_bot` and reports `FromBot`
before reading the message. Dependabot cannot be made to emit a matching trailer
(dependabot-core#3480, closed not-planned), so the pass is an exemption.

The retired `.github/workflows/dco.yaml` exempted **one** bot, matched on an
unforgeable login and reviewable in-repo. `dco2` exempts **every** bot-authored
commit, and its whole config surface — `allowOverrideAction`,
`allowRemediationCommits`, `require.members` — has no key to narrow or disable
that. The swap bought server-side evaluation and widened the bot exemption; both
are true.

The other default the swap introduced is closed rather than accepted. `dco2`
renders a *Set DCO to pass* button on a failed check unless told not to, and the
retired workflow had no equivalent one-click path, so `.github/dco.yml` now sets
`allowOverrideAction: false`. Two things follow that are easy to get wrong:

- **It takes effect only from `main`.** `dco2` reads `.github/dco.yml` from the
  default branch, so this had no effect on the pull request that added it, and a
  pull request cannot re-enable the button for itself. Verify after merge, not
  before, by looking at a failed `DCO` check's details page.
- **It is not a security boundary**, and upstream says so — anyone with write
  access can edit the file. It stops the button being reached for while trying to
  land something, and makes clearing a red `DCO` a reviewable commit instead. The
  residual insider risk is recorded as accepted in
  `docs/internal/threat-model.md`.

The bot and merge exemptions above have no such switch; those stay accepted.

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
- `CodeQL gate` blocks a pull request that introduces a new alert, but only above a
  severity floor and only for findings new relative to `main`. Read
  [Static analysis](#static-analysis) before quoting it as a control, and do not swap
  it for `Analyze (go)` / `Analyze (actions)`. Unlike the `CodeQL` context it
  replaces, it does report on fork and Dependabot pull requests.

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

**The residual gap, and which half is now closed.** A fork pull request gets no
pre-merge Guardian/SCA verdict: those scan jobs need Environment secrets, forks
don't get them, so a fork PR merges unscanned by FOSSA and Prisma and is caught
only by the scan on push to `main`. Note that "require approval for outside
collaborators" (GitHub Actions Permissions section) protects the *secrets*, not
the *verdict* — an approved fork run still can't scan. The repository is now
public with `allow_forking` on, so this is the normal contribution path rather
than an edge case. The two halves of the gap have different answers (SOL-153086):

**Vulnerabilities — a preventive control exists, and closes the gap once the
context is registered.** `Dependencies free of high advisories` (`ci-pr.yaml` job
`dependency_review`) fails a pull request that introduces a dependency with a
known high or critical advisory. It reads the Dependency Review API through
`GITHUB_TOKEN` against the dependency graph, which is always on and free for a
public repository, so it needs no third-party secret and reports on fork pull
requests. It is the only control here capable of *prevention* — everything else in
this section detects after merge — but capability is not enforcement: until the
context is registered on the ruleset it goes red without blocking anything, so
read this half as closed only from that point on. Four caveats on how far to read
a green verdict:

- **It is registered as required or it is nothing.** See the warning in the
  required-checks section above. As of this writing it is not.
- **It reads the diff, not the tree.** It catches a dependency the pull request
  *adds* that is already known-bad. It does not catch an advisory published
  tomorrow against a dependency already in `go.mod` — that stays with the scan on
  `main`. So a green verdict means "this pull request adds nothing newly
  known-bad", not "the tree is clean". That containment is also what makes the
  check safe to require: a pull request touching no manifest has no dependency
  changes and passes, so a fresh advisory does not turn every open pull request
  red.
- **It sees all three `go.mod` files**, not just the server binary's closure.
  Confirmed via the repository SBOM: `solace.dev/go/messaging` is required only by
  `test/e2e-common/broker-driver/go.mod` and is present in the graph. That is
  wider coverage than `licenses-check.sh`, which walks
  `go list -deps ./cmd/server` and would never see an indirect entry outside it.
- **It covers Go modules, not workflow actions.** Roughly half the dependency
  changes this repository produces are the `actions` ecosystem, and every action
  is SHA-pinned, so advisory matching by version range does not resolve for them.
  zizmor's `known-vulnerable-actions` is the control for that half, and
  `workflow-lint.yaml` runs `--no-online-audits`, which switches it off. A
  scheduled non-blocking zizmor run is the way to pick it up.

Three inputs beyond `fail-on-severity` are set explicitly rather than inherited,
so the gate cannot change behaviour when the action's defaults do. This is the
authoritative copy of why; `ci-pr.yaml` carries one line each and points here.

- `license-check: false` — the licence policy lives in FOSSA. With no allow/deny
  list the licence half is inert either way, but setting it makes
  "vulnerabilities only" a reviewable decision instead of an accident of defaults,
  and keeps a second driftable copy of that policy out of the repository.
- `show-openssf-scorecard: false` — it calls deps.dev live. A gate whose verdict
  can move with no commit behind it is not a gate; the same reasoning has
  `workflow-lint.yaml` running zizmor with `--no-online-audits`.
- `comment-summary-in-pr: never` — already the default, pinned anyway. `always`
  and `on-failure` require `pull-requests: write`, which a fork pull request's
  token does not carry, so a later change here would fail on precisely the case
  the job exists for. The verdict goes to the job summary instead.

**Licences — an accepted gap, recorded rather than closed.** A fork pull request
introducing a dependency under a denied licence still merges. FOSSA catches it on
push to `main` and again at the release tag. Accepted on three grounds:

- A licence problem has no adversary behind it. It is a compliance defect —
  needs fixing before release, not exploitable in the meantime.
- The release gate already blocks shipping past FOSSA licensing findings, which
  is where it actually bites.
- **The compensating control is the inventory diff.** `Third-party licenses
  current` is required, needs no secret, and reports on fork pull requests. A
  contributor who adds a dependency without regenerating
  `THIRD_PARTY_LICENSES.md` gets a red check; one who does regenerate it puts the
  dependency by name, with its licence, into the diff a reviewer reads. There is
  no equivalent human-visible signal for "this version has a CVE", which is why
  that half got automation and this one did not.

A licence allow-list on `dependency-review-action` would close it at the pull
request, and was the original proposal. Rejected on maintenance: it is a second
expression of a policy that already lives in FOSSA, and two copies drift; it needs
a named owner and a Legal sign-off path, and neither exists. `@SolaceProducts/dax-developers`
via CODEOWNERS guarantees someone approves the diff, not that anyone consults
Legal. Revisit when someone will own the list.

`govulncheck` was weighed as an alternative to `dependency-review-action` rather
than a complement — it also needs no secret, and its reachability analysis means
far fewer false positives, at the cost of missing a vulnerability in a dependency
the code does not call yet. Either is defensible; one was chosen, and running both
would mean two verdicts to reconcile on every dependency bump.

Two human controls still stand, and both need someone to actually do them:

- **Read `go.mod` and `go.sum`** in any fork pull request that touches them. The
  automation covers *known high and critical* advisories in the diff; it says
  nothing about a dependency that is merely unmaintained, unnecessary, or
  typosquatting a real one.
- **Read the `.github/` diff too.** Moving to `pull_request` means a fork pull
  request supplies the workflow definitions that run for it. It cannot reach a
  secret and its token is read-only, so this is not a credential-theft path, but a
  pull request can still weaken its own checks — including `Guardian scan gate`
  and `dependency_review`, both of which run from the pull request's own ref.
  `DCO` is the exception: it is a GitHub App evaluating commits server-side, not a
  workflow the pull request supplies, so a pull request cannot edit the thing that
  judges it.

**Who picks up a red supply-chain scan on `main`.** Nobody, by rota or
assignment. That is the honest answer today and it is written here rather than
left to be assumed, because a detection control recorded as unwatched is more
useful than one believed to be watched. The specifics:

- The failure lands on an already-merged commit. `guardian-scan.yaml` on `push`
  to `main` goes red roughly ten minutes after the merge, and `guardian-gate`
  files its findings through Guardian's Jira reporting.
- **No alert routes to a person.** No assignee, no Slack destination, no rota
  owns the red `main` run. GitHub emails the commit author on a failed workflow
  run by default, which for a merged fork pull request is the external
  contributor, not a maintainer.
- **So the standing convention is: whoever merges a dependency-touching pull
  request watches the next `main` run themselves.** For a fork pull request, treat
  that as part of merging it.
- **To fix this properly**, route `guardian-scan.yaml` failures on `main` to a
  team destination and name the rota that reads it. Until that exists, do not
  count the `main` scan as a control anyone is watching.

Also worth knowing: on a same-repo pull request FOSSA runs in diff mode and in
REPORT (not BLOCK), so a green `Guardian scan gate` there is not a clean full scan
— the hard gate is on push to `main` and at the release tag.

Nobody has ever opened a fork pull request against this repository, so none of the
above is observed on a real external contribution. `CHANGELOG updated` should be
fine: `ci-pr.yaml` gives that job only `contents: read` and it reads no secrets.
`CodeQL gate` should be fine on a fork ref, and this is the one item here with
outside evidence rather than reasoning: on 19 August 2026, fork pull requests
against `cli/cli` (advanced setup, the same `pull_request` trigger shape as
`codeql.yml`) carried real verdicts from both `Analyze` jobs, so the SARIF upload
is not blocked by the read-only-token downgrade. The `CodeQL` context it replaces
was the opposite — default setup analyzes no fork ref at all. Copilot review is
inconsistent even on same-repo pull requests, so do not count on it. Verify
`Guardian scan gate`, `CHANGELOG updated`, `CodeQL gate`, `Dependencies free of
high advisories`, and the `lint` job's annotation behavior on a read-only token
against a real fork pull request before treating any of them as a gate.

`Dependencies free of high advisories` is the one on that list whose fork
behaviour is load-bearing rather than incidental, since closing the vulnerability
half rests on it. Four things were checked before it was written down, because
"the API needs a token the fork does not have" is the failure mode that would make
this control imaginary:

- **The API returns real data here.**
  `dependency-graph/compare/v0.6.0...v0.7.1` → 33 changes, ecosystems `gomod` and
  `actions`.
- **Read access is enough; push access is not required.** The same endpoint
  returns data on repositories where the calling account has `push: false` and
  `pull` only. This is the property a fork pull request's read-only
  `GITHUB_TOKEN` depends on.
- **A fork pull request's head SHA resolves in the base repository.** Checked
  against merged fork pull requests on `prometheus/prometheus`, where the head
  commit lives in the contributor's fork: #19276 → 701 changes, #19269 → 4. GitHub
  fetches the head commit into the base repository, and the dependency graph
  computes over it.
- **The gate fails when it should.** The pinned action was run against a range
  that adds dependencies carrying high and critical advisories, with the exact
  inputs from `ci-pr.yaml`: exit 1, `Dependency review detected vulnerable
  packages`. `warn-only: true` on the same data exits 0, which is what rules out a
  configuration error dressed up as a finding.

What remains unverified is only what no test can reach without a fork: no fork
pull request has ever run in *this* repository, and the "require approval for
outside collaborators" hold below means the first one's checks do not start until
a maintainer clicks approve. Confirm the check reports on the first real fork pull
request before registering the context.

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
- ✅ **Required status checks corrected** per the Branch Protection section above
  — this document corrected 2026-08-17 (SOL-153190) to match the live ruleset,
  which was itself updated 2026-08-12. The `main-protection` ruleset now requires
  all fourteen registered contexts in the table above, including `Guardian scan gate`
  (from `guardian-scan.yaml`) in place of the retired `FOSSA Scan` / `FOSSA Scan /
  SCA Scan` contexts, with `strict_required_status_checks_policy: true` and an
  **empty bypass-actor list** — no admin override exists. Re-verify against the
  live ruleset rather than trusting this line. The list-rulesets endpoint
  returns only `id`/`name` — no embedded rule detail — so resolving by name
  needs two calls, not one:
  ```bash
  RULESET_ID=$(gh api repos/OWNER/REPO/rulesets --jq '.[] | select(.name=="main-protection") | .id')
  gh api "repos/OWNER/REPO/rulesets/$RULESET_ID" \
    --jq '.rules[] | select(.type=="required_status_checks")
          | .parameters.required_status_checks[] | "\(.context)\t\(.integration_id // "UNPINNED")"'
  ```
  This resolves the ID at call time instead of hardcoding it, so it keeps working if the ruleset is ever recreated under a new one.

  > **Historical note, kept for context.** Before this was registered, the
  > precondition was to confirm `Guardian scan gate` was green on a real pull
  > request first: the `gate` job fails closed by design, so requiring it while
  > the scan itself was broken would have turned "scanning is broken" into "no
  > pull request can merge." That precondition was met before registration.
  > **There is no "branch protection holds zero required contexts" state
  > anymore** — every one of the fourteen contexts, `Guardian scan gate`
  > included, now blocks a merge if it is red or never reports, with nothing to
  > bypass it. One exception, added later: `CodeQL` can conclude `neutral`, which
  > GitHub counts as passing. Its replacement `CodeQL gate` cannot — it fails on a
  > missing analysis rather than concluding `neutral` — so that exception retires
  > with the migration; see [Static analysis](#static-analysis). A scan outage today blocks everyone, which is what makes the "Who
  > picks up a red supply-chain scan on `main`" gap earlier in the Branch
  > Protection section (above) worth reading, not less.
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
- ✅ **"Allow GitHub Actions to create and approve pull requests" turned off.**
  (SOL-152958)
- ⬜ **Fork pull request workflows set to require approval for all outside
  collaborators.** The default is weaker; see the GitHub Actions Permissions
  section. No GitHub REST API exposes this setting, so it has to be flipped by
  hand in the UI by someone with org-admin access — tracked under SOL-152960.
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
- support@solace.com
- Solace DevRel team
