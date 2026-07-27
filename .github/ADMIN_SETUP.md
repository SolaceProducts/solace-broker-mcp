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
    - `e2e`
    - `e2e-oauth`
    - `DCO sign-off`
- ✅ **Require conversation resolution before merging**
- ✅ **Do not allow bypassing the above settings** (enforces rules for admins too)
- ⬜ Require signed commits (optional — GPG signing is separate from DCO)

> **`DCO sign-off` is not optional.** It stands in for a contributor licence
> agreement, and a check that is not in the list above enforces nothing. It also
> has to be *required* rather than merely present: `pull_request` workflows run
> from the PR's own ref, so a fork can delete the job — a required check that
> never reports blocks the merge, an unrequired one does not.

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
  - Require status checks: build, lint, e2e, e2e-oauth, `DCO sign-off`
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
