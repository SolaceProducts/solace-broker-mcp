# Repository Admin Setup Checklist

This document contains tasks that require **repository admin access**. These should be completed before or immediately after making the repository public.

**Admin needed**: @andreaross or repository administrators

---

## Repository Settings

### Basic Information (M1: Repository Metadata)

Navigate to: **Settings → General → About**

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

## Features (M2: GitHub Discussions)

Navigate to: **Settings → General → Features**

**Enable:**
- ✅ **Discussions** - Enable this feature
- ✅ Issues (already enabled)
- ✅ Preserve this repository (mark as important)
- ✅ Sponsorships (if applicable)

**After enabling Discussions**, create categories:

1. Navigate to **Discussions → Categories** (⚙️ icon)
2. Create these categories:

| Category | Description | Format |
|----------|-------------|--------|
| **Announcements** | Release announcements and project updates | Announcement (maintainers only) |
| **Q&A** | User questions and answers | Q&A (enable "Mark as answer") |
| **Ideas** | Feature brainstorming and feedback | Open discussion |
| **Show and Tell** | Community showcases and use cases | Open discussion |
| **General** | Everything else | Open discussion |

3. Pin a welcome post in Announcements:
   ```markdown
   # Welcome to Solace Broker MCP Server Discussions!

   Use this space to:
   - Ask questions (Q&A category)
   - Share ideas for features (Ideas)
   - Show off your integrations (Show and Tell)

   For bug reports and feature requests, please use [Issues](../issues) instead.
   ```

---

## Security Settings

Navigate to: **Settings → Security → Code security and analysis**

**Enable:**
- ✅ **Dependabot alerts** - Automatic vulnerability scanning for Go dependencies
- ✅ **Dependabot security updates** - Auto-create PRs for security patches
- ✅ **Secret scanning** - Detect accidentally committed credentials
- ⚠️ **CodeQL analysis** (optional) - Advanced code scanning
  - May be overkill for this project size
  - Adds ~2-3 minutes to CI runs
  - Recommend enabling if repo will be heavily used

**Configure Dependabot** (Settings → Security → Dependabot):

Create `.github/dependabot.yml` (can be done in a PR, no admin needed):
```yaml
version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/"
    schedule:
      interval: "weekly"
    open-pull-requests-limit: 5
    reviewers:
      - "andreaross"
    labels:
      - "dependencies"
      - "automated"
```

---

## Branch Protection Rules

Navigate to: **Settings → Branches → Branch protection rules → Add rule**

**Branch name pattern**: `main`

**Protect matching branches:**
- ✅ **Require a pull request before merging**
  - ✅ Require approvals: **1**
  - ✅ Dismiss stale pull request approvals when new commits are pushed
  - ⬜ Require review from Code Owners (enable if CODEOWNERS file added)
- ✅ **Require status checks to pass before merging**
  - ✅ Require branches to be up to date before merging
  - Select required checks:
    - `build`
    - `lint`
    - `e2e-basic-mcp`
    - `e2e-oauth`
    - `DCO sign-off`
    - `DCO check self-test`
- ✅ **Require conversation resolution before merging**
- ✅ **Do not allow bypassing the above settings** (enforces rules for admins too)
- ⬜ Require signed commits (optional — GPG signing is separate from DCO)

> **The two DCO contexts are not optional, and the exact strings matter.** DCO
> stands in for a contributor licence agreement, so the repository is not covered
> until both are registered. A check that is not required enforces nothing.
>
> | Check | Source | Gates on |
> |-------|--------|----------|
> | `DCO sign-off` | `dco.yaml` job `dco` | a sign-off on every commit the PR adds |
> | `DCO check self-test` | `ci-pr.yaml` job `dco_selftest` | the gate's own logic still working |
>
> ⚠️ **If you rewrite this list, carry both rows over.** `DCO sign-off` *is* the
> control; dropping it removes it. `DCO check self-test` is a regression guard for
> trusted pull requests only — it runs on `pull_request`, so a fork can rewrite
> the test in its own PR, which is exactly why the gate itself does not live
> there. PR #220 replaces this section with a table of its own; whichever lands
> second must keep both rows.
>
> Both strings are the jobs' `name:` values verbatim. They are plain jobs, not
> reusable-workflow calls, so each produces exactly one context with no ` / `
> suffix. Verified from the Checks API on this repo: the `changelog` job (id
> `changelog`, `name: CHANGELOG updated`) surfaces as the context `CHANGELOG
> updated`, and the job id never appears. Not yet verified on a live run of
> `dco.yaml`, because `pull_request_target` runs the base ref's copy and the file
> is not on `main` yet — **confirm both strings against the first PR after it
> merges**:
> `gh api repos/OWNER/REPO/commits/<head-sha>/check-runs --jq '.check_runs[].name'`
>
> Why the exact string matters: `FOSSA Scan` is required today and enforces
> nothing. Two different workflows name a job `FOSSA Scan`, and on a pull request
> the one that reports is `build-and-test.yml`'s, which is gated on `push` to the
> default branch and therefore reports **skipped**. GitHub counts a skipped
> required check as satisfied. The scan that actually runs on pull requests
> reports as `FOSSA Scan / SCA Scan` and is not required. Requiring the wrong
> string would make DCO decorative in exactly the same way.
>
> `DCO check self-test` needs to be required for a different reason: the gate runs
> the *base ref's* copy of the script, so a PR that breaks `dco-check.sh` shows the
> self-test red and the gate green. Unless the self-test blocks, that PR merges and
> the gate is broken for everything after it.
>
> **Rollout order: merge the PR first, then register the checks.** Registering
> `DCO sign-off` first deadlocks the PR that introduces it —
> `pull_request_target` runs the base ref's workflows, so the check cannot report
> until the file is on `main`.
>
> If you set *Require approval for all outside collaborators* (Settings → Actions
> → General), fork PR runs wait for a maintainer and their checks read **pending**
> rather than failing. `pull_request_target` is not exempt from this. Pending
> fails closed, so nothing slips through. The failure mode to avoid is concluding
> the required-checks list is misconfigured and dropping a DCO row from it.
> Approve the workflow run instead; do not edit the required list.

**Rules applied to admins:**
- ⬜ Allow admins to bypass (leave UNCHECKED - admins should follow same rules)

---

## GitHub Actions Permissions

Navigate to: **Settings → Actions → General → Workflow permissions**

**Recommended:**
- ⚪ Read and write permissions (needed for release workflow to create releases)
- ✅ Allow GitHub Actions to create and approve pull requests (for Dependabot)

**Alternative (more restrictive):**
- ⚪ Read repository contents and packages permissions
- Manually grant write access to release workflow via repo secrets

---

## Visibility Settings

Navigate to: **Settings → General → Danger Zone**

**BEFORE making public, ensure:**
- ✅ All CRITICAL items resolved (LICENSE, CONTRIBUTING.md, CODE_OF_CONDUCT.md)
- ✅ All HIGH items resolved (SECURITY.md, templates, CHANGELOG)
- ✅ All secrets removed from git history (`git log --all -S "password"`)
- ✅ `.gitignore` properly configured (`.env`, `broker-config.yaml`)
- ✅ No sensitive data in issues, PRs, or discussions
- ✅ First release (v0.1.0) tagged and published

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

## Rulesets (Alternative to Branch Protection)

Navigate to: **Settings → Rules → Rulesets**

GitHub Rulesets are the new way to configure branch protection (more flexible than legacy rules). If available:

**Create ruleset**: "Protect main branch"
- Target: `main` branch
- Bypass: Nobody (or specific users/teams)
- Rules:
  - Require pull request with 1 approval
  - Require status checks: build, lint, e2e, e2e-oauth, `DCO sign-off`, `DCO check self-test`
  - Block force pushes
  - Block deletions

---

## Post-Publication Checklist

After repository is made public:

- [ ] Verify all badges in README.md render correctly
- [ ] Check that CODE_OF_CONDUCT and CONTRIBUTING show in sidebar
- [ ] Test issue template flow (create a test issue, then close it)
- [ ] Test PR template (create a test PR, then close it)
- [ ] Verify SECURITY.md appears in Security tab
- [ ] Submit to Go package indexes (if applicable)
- [ ] Announce on Solace Community forum
- [ ] Share on relevant channels (Twitter, LinkedIn, etc.)
- [ ] Add to MCP server registry (if one exists)

---

## Monitoring & Maintenance

**Weekly:**
- Review open issues and PRs (target <7 days for first response)
- Merge Dependabot security updates
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
