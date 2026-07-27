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

- `.github/CONTRIBUTING.md:49`
- `.github/CONTRIBUTING.md:259`
- `.github/ISSUE_TEMPLATE/config.yml:4` (the issue-template chooser)

`.github/ISSUE_TEMPLATE/feature_request.yml:122` also says "issues and
discussions" in prose. Repointing all of them at the Solace Community category is
tracked separately. Confirm that landed, or expect broken links from day one.

---

## Security Settings

Navigate to: **Settings → Security → Code security and analysis**

Current state, read from the repository API:

| Setting | Now | Action |
|---------|-----|--------|
| Dependabot alerts | On | None |
| Dependabot security updates | On | None |
| Secret scanning | **Off** | **Enable** |
| Secret scanning push protection | **Off** | **Enable** |
| CodeQL | Running - `Analyze (go)` passes on every PR | Confirm on the settings page |

Enable both secret-scanning settings, not just the first. Alerts find credentials
already committed; push protection rejects the next one before it lands. Both are
free on public repositories.

On CodeQL: a check named `Analyze (go)` runs and passes on every PR, produced by
the code-scanning default setup, while the repository API reports Code Security as
disabled. Confirm which configuration is producing it on the settings page before
relying on it, and re-check after the visibility change.

**Dependency updates use Renovate, not Dependabot.**

`.github/renovate.json` scopes Renovate to exactly one dependency: the pinned
Claude Code CLI version in the LLM e2e harness. Go modules are deliberately
excluded to prevent runaway PRs, and are bumped by hand.

- Do **not** create `.github/dependabot.yml`. It would contradict that scoping.
- Dependabot's role here is alerts and security-only updates. Those do not
  conflict with Renovate and are already on.

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

`.github/CODEOWNERS` exists (`* @SolaceDev/dax-developers`), so Code Owners review
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
PRs #213, #216, and #217:

| Check | Source | Gates on |
|-------|--------|----------|
| `lint` | `build-and-test.yml` job `lint` | golangci-lint |
| `build` | `build-and-test.yml` job `build` | build, vet, race tests |
| `e2e-basic-mcp` | `build-and-test.yml` | E2E suite |
| `e2e-oauth` | `build-and-test.yml` | E2E suite |
| `e2e-monitoring` | `build-and-test.yml` | E2E suite (known flaky fixture; rerun the job before investigating) |
| `e2e-management` | `build-and-test.yml` | E2E suite |
| `e2e-action` | `build-and-test.yml` | E2E suite |
| `FOSSA Scan / SCA Scan` | `ci-pr.yaml` job `fossa_scan` | Licensing policy and critical/high vulnerabilities, diffed against the base branch |
| `CHANGELOG updated` | `ci-pr.yaml` job `changelog` | Advisory today; see note below |

⚠️ **Do not select `FOSSA Scan`.** It looks like the right entry and is not. A
check by that exact name comes from `build-and-test.yml`, where the job carries
`if: github.event_name == 'push' && github.ref_name == <default branch>`, so on
every pull request it reports **skipped**, and GitHub counts a skipped check as
passing. Requiring `FOSSA Scan` therefore enforces nothing. The context that
gates is `FOSSA Scan / SCA Scan`: the `ci-pr.yaml` caller job (`name: FOSSA Scan`)
plus the inner job of the reusable workflow it calls (`name: "SCA Scan"`), which
exits non-zero on findings because `.github/workflow-config.json` sets both FOSSA
modes to `BLOCK`. Reusable-workflow jobs always surface as
`<caller job name> / <inner job name>`, never as the caller name alone.

Four more notes on the list:

- `CHANGELOG updated` cannot fail today. `ci-pr.yaml` sets
  `CHANGELOG_GATE_MODE: advisory`, so the script warns and exits 0. Requiring it
  makes the check's presence a merge precondition now, so flipping the mode to
  `blocking` later needs no ruleset change.
- On a pull request, FOSSA runs in diff mode (`enable_diff_mode` is true for
  `pull_request` events). It blocks on findings that are new relative to the base
  branch, not on the full dependency inventory. That is what keeps `main` from
  entering an irregular state; do not read a green PR as a clean full scan.
- `Analyze (go)` (CodeQL) is available and passes on every PR. Add it if you want
  code scanning to block merges; it is not in the list above because that is a
  policy call, not a correctness fix.
- Do **not** require `run-pull-request-checks / *`. Those come from
  `transition_on_merge.yaml`, which runs on `pull_request: closed`, and the check
  name varies with the Jira key (e.g. `... Vault and JIRA Operations (SOL-152328)`).

🚨 **CI cannot currently green-light a fork pull request.** Two separate problems,
both of which need a CI change. Tracked separately.

1. **Seven checks never appear.** `build-and-test.yml` triggers on `push`, not
   `pull_request`. A fork contributor's push fires in their fork, so `lint`,
   `build`, and the five `e2e-*` checks never report on this repository's commit.
   Those required contexts stay pending forever.
2. **`FOSSA Scan / SCA Scan` fails.** `ci-pr.yaml` passes `use_vault: true` and
   `secrets.VAULT_URL`. GitHub withholds secrets from `pull_request` runs
   triggered by a fork, so the reusable workflow finds an empty Vault URL and
   exits 1. It runs, and it goes red.

`CHANGELOG updated`, `Analyze (go)`, and Copilot review do run normally on fork
PRs. Until the CI fix lands, merge community contributions by pushing the branch
into this repository yourself.

---

## GitHub Actions Permissions

Navigate to: **Settings → Actions → General → Workflow permissions**

**Workflow permissions:**
- ⚪ Read and write permissions (needed for release workflow to create releases).
  Currently set.
- 🚨 **Allow GitHub Actions to create and approve pull requests: turn this OFF.
  It is currently ON.** Combined with a write `GITHUB_TOKEN` and a `main` ruleset
  that needs exactly one approval and has an empty bypass list, a workflow can
  approve a pull request and satisfy the only human gate on `main`. Nothing here
  needs it: Renovate opens PRs with its own GitHub App installation token, and no
  workflow in this repository opens or approves PRs.

**Alternative (more restrictive):**
- ⚪ Read repository contents and packages permissions
- Manually grant write access to release workflow via repo secrets

**Fork pull request workflows**

Navigate to: **Settings → Actions → General → Fork pull request workflows from
outside collaborators**

Pick the approval posture before the repo goes public. The default on a public
repo is "require approval for first-time contributors", which is the weakest of
the three. This repository allows all actions and does not require SHA pinning, so
"require approval for all outside collaborators" is the safer default until that
changes.

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
- ⬜ **Required status checks corrected** per the Branch Protection section above,
  including the `FOSSA Scan / SCA Scan` swap. The ruleset still holds the old
  list.
- ⬜ **"Allow GitHub Actions to create and approve pull requests" turned off.**
  Still on.
- ⬜ **Secret scanning and push protection enabled.** Both still off.
- ⬜ **Decide whether to cut a release first.** v0.1.0 through v0.5.0 are already
  tagged and published (v0.5.0 on 2026-07-10), so this is not about a first
  release existing. `[Unreleased]` in `CHANGELOG.md` currently carries BREAKING
  entries, so the public repo's newest release would not describe what is on
  `main`.
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

**Recommended:**
- ✅ Allow merge commits (default)
- ✅ Allow squash merging (useful for cleaning up commit history)
- ✅ Allow rebase merging
- ✅ Always suggest updating pull request branches
- ✅ Automatically delete head branches (keeps repo clean)

**Default commit message:**
- Squash: "Pull request title and description"
- Merge: "Pull request title"

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
