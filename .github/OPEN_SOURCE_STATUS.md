# Open Source Readiness Status

**Last Updated**: 2026-07-27
**Originating PR**: #15 (aross/SOL-149029-open-source-compliance), merged
**Last revised by**: SOL-152405 (support-channel decision, admin-checklist accuracy)

---

## Completion Status

### ✅ CRITICAL (All Complete)

All blocking issues for public release are resolved:

- [x] **C1: LICENSE file** - Apache 2.0 license in place
- [x] **C2: CONTRIBUTING.md** - Comprehensive contribution guidelines with DCO
- [x] **C3: CODE_OF_CONDUCT.md** - Contributor Covenant 2.1 adopted

### ✅ HIGH Priority (All Complete)

All high-priority items are complete:

- [x] **H1: SECURITY.md** - Vulnerability disclosure policy with security best practices
- [x] **H2: Issue Templates** - Bug report and feature request templates
- [x] **H3: PR Template** - Comprehensive pull request checklist
- [x] **H4: CHANGELOG.md** - Keep a Changelog format with v0.1.0 history
- [x] **H5: Release Process** - Documented in RELEASING.md (workflow already exists from PR #14)

### ⏳ MEDIUM Priority (Requires Admin Access)

These items are **documented** but require repository admin to configure:

- [ ] **M1: Repository Metadata** - See ADMIN_SETUP.md section "Repository Settings"
  - Description, topics, website URL
  - Admin needed to configure in Settings → General → About
- [ ] **M2: Support channels** - See ADMIN_SETUP.md section "Features"
  - GitHub Discussions is **not** being enabled. Support runs on two channels:
    GitHub Issues for bugs and features, and a dedicated Solace Community
    (Discourse) category for questions and discussion. Vulnerabilities go through
    private disclosure per SECURITY.md.
  - This supersedes the earlier plan in this file to open Discussions.
  - Remaining work: stand up the Solace Community category and repoint the
    `/discussions` links in `.github/CONTRIBUTING.md` and
    `.github/ISSUE_TEMPLATE/config.yml`. Tracked separately.
- [x] **M3: README Badges** - Already added in first commit (build, license, Go version, CoC)

---

## Exceptions

Items where the checklist calls for a control this repository does not fully
meet, recorded per section 5 of the Open Source Solace Software Checklist. Each
row needs four approvers named before the repository goes public.

### E1: Non-routable author addresses in seven pre-existing commits

**Ticket:** SOL-152902
**Recorded:** 2026-08-06

Seven commits already on `main` carry author emails whose domain is a
developer-machine name rather than a routable address: two contributors, two
machines, dated 2026-05-06 through 2026-07-07. Every other commit on `main` uses
`@solace.com` or `@users.noreply.github.com`.

The per-commit list of SHAs and addresses lives on SOL-152902, not here. This
file is written to be published, so reproducing the addresses in it would copy
the exposure it records out of commit metadata and into a tracked, indexable,
code-searchable file. Read the current state from the history itself:

```bash
git log --format='%h %an <%ae> %ad' --date=short | grep -E '\.sol-local>|\.local>'
```

**Decision:** accept, do not rewrite.

Rewriting these seven commits would change the SHA of every commit after them —
close to the whole history — and break every existing reference to a commit SHA
in Jira, PR discussions, and the CHANGELOG. The exposure is one internal dev
hostname and one contributor's laptop name, both already attached to those
contributors' own commits and already visible to anyone with access to the
repository today. The cost-to-value ratio favours accepting.

**What we do instead:**

- `.github/scripts/commit-identity-check.sh` fails any future PR whose commits
  carry an author or committer email on a non-routable domain (`.local`,
  `.sol-local`, `.internal`, `.lan`, or a bare hostname). Denylist rather than
  allowlist so outside contributors with ordinary addresses are not blocked. The
  gate is forward-only — it walks the PR's commits, not the history already on
  `main`.
- `.github/scripts/commit-identity-check.test.sh` covers the pass and fail paths
  plus a mutation probe, so the gate is verified on every PR rather than trusted.
- `.github/CONTRIBUTING.md` states the identity requirement and the fix
  commands, next to the DCO enforcement section it already describes.
- Both affected contributors are notified before the repository flips public.

---

## What's in PR #15

### New Files Created

**Core Compliance:**
- `LICENSE` - Apache 2.0 official text (11.3 KB)
- `.github/CONTRIBUTING.md` - Contribution guidelines (9.6 KB)
- `.github/CODE_OF_CONDUCT.md` - Contributor Covenant 2.1 (5.4 KB)

**Security & Templates:**
- `.github/SECURITY.md` - Vulnerability disclosure policy (7.2 KB)
- `.github/ISSUE_TEMPLATE/bug_report.yml` - Structured bug report form (4.3 KB)
- `.github/ISSUE_TEMPLATE/feature_request.yml` - Structured feature request form (4.4 KB)
- `.github/ISSUE_TEMPLATE/config.yml` - Issue template configuration (525 B)
- `.github/PULL_REQUEST_TEMPLATE.md` - PR checklist template (3.3 KB)

**Process Documentation:**
- `CHANGELOG.md` - Version history following Keep a Changelog (5.8 KB)
- `.github/ADMIN_SETUP.md` - Admin configuration guide (8.0 KB)
- `.github/RELEASE_GUIDE.md` - Release process documentation (9.3 KB; since superseded by the root `RELEASING.md`)

### Modified Files

**Copyright Headers:**
- All 21 Go source files updated with Apache 2.0 headers

**Documentation:**
- `README.md` - Added badges, Contributing section, Security section, License section

**Total Changes:**
- **33 files changed**
- **2,231 insertions** (lines added)
- **4 deletions** (lines removed)

---

## What Works Without Admin

✅ **All files created** - No admin access needed
✅ **Templates will work** - GitHub automatically recognizes them
✅ **SECURITY.md visible** - GitHub shows in Security tab
✅ **COC and Contributing** - GitHub shows in sidebar
✅ **PR template active** - Applies to all new PRs
✅ **Issue templates active** - Applies to all new issues

## What Requires Admin

These settings must be configured by repository admin:

### 1. Repository Metadata (15 minutes)
- Description and website URL
- Topics for discoverability
- See: ADMIN_SETUP.md → "Repository Settings"

### 2. Support Channels (5 minutes)
- Leave GitHub Discussions **off**
- Confirm Issues is on
- See: ADMIN_SETUP.md → "Features"

### 3. Branch Protection (10 minutes)
- Require PR reviews before merge
- Require CI checks to pass
- **`Guardian scan gate`** (from `guardian-scan.yaml` job `gate`) is the
  supply-chain context, and the only safe one to require: it always reports.
  Two nearby names must stay out of the list. `Guardian scan` is the *workflow's*
  name and has never been a check at all, so requiring it blocks every merge
  forever. `Guardian gate` is a real job gated on `is_trunk`, so it is always
  skipped on a pull request and GitHub counts skipped as passing — a gate that
  enforces nothing. `.github/ADMIN_SETUP.md` explains both failure modes.
- **`DCO` is required**, served by the CNCF `dco2` GitHub App. DCO is the control
  the project carries in place of a contributor licence agreement, and an
  unrequired check enforces nothing. `.github/ADMIN_SETUP.md` has the exact
  string and the reason.
- Prevent force pushes to main
- See: ADMIN_SETUP.md → "Branch Protection"

### 4. Security Features (5 minutes)
- Secret scanning and secret-scanning push protection are already on, as are
  validity checks and non-provider patterns. Confirm, do not re-enable
- Dependabot alerts and Dependabot security updates are already on. Scheduled
  Go module and GitHub Actions updates run on Dependabot too
  (`.github/dependabot.yml`). Renovate can't be enrolled for a public repo
  under the org's current system, so it stays scoped to just the pinned Claude
  Code CLI version (`.github/renovate.json`). Dependabot's PRs cannot carry a
  `Signed-off-by` trailer; the `dco2` App passes them anyway, confirmed on
  PR #289
- CodeQL runs on every PR, but the repository API reports Code Security as
  disabled; confirm the configuration rather than assuming it
- See: ADMIN_SETUP.md → "Security Settings"

### 5. Actions Settings (5 minutes)
- Turn off "Allow GitHub Actions to create and approve pull requests" (currently on)
- Set fork pull request workflows to require approval for all outside collaborators
- See: ADMIN_SETUP.md → "GitHub Actions Permissions"

### 6. Make Repository Public (5 minutes)
**ONLY AFTER:**
- Admin tasks in the preceding sections are complete
- A release has been cut that covers what is on `main`. The newest release is
  v0.6.0 (tagged 2026-07-28), and `main` has moved on since with `[Unreleased]`
  entries in `CHANGELOG.md`, so v0.6.0 does **not** describe `main` today. Cut one
  before the flip or the first thing a new user downloads is behind the code they
  are reading.
- See: ADMIN_SETUP.md → "Visibility Settings"

**Total admin time**: ~45 minutes

---

## Open Source Maturity Progress

This table is a historical record of PR #15 (2026-04-24), not current state. As of
2026-07-29 one gap remains behind the Security row: "Allow GitHub Actions to
create and approve pull requests" is still on. Secret scanning and its push
protection have since been enabled. Do not cite the following score as go/no-go
evidence without re-checking the live settings.

| Category | Before PR #15 | After PR #15 | Target |
|----------|---------------|--------------|--------|
| Legal | 0/5 ❌ | **5/5 ✅** | 5/5 |
| Community | 0/5 ❌ | **5/5 ✅** | 5/5 |
| Security | 2/5 ⚠️ | **4/5 ✅** | 5/5 |
| Documentation | 4/5 ✅ | **5/5 ✅** | 5/5 |
| Code Quality | 5/5 ✅ | **5/5 ✅** | 5/5 |
| Release Process | 1/5 ❌ | **4/5 ✅** | 5/5 |
| Discoverability | 0/5 ❌ | **3/5 ⚠️** | 5/5 |

**Overall Score at the time of PR #15**: 31/35 (89%) - **Growth Stage** ✅

**Target Achieved at the time of PR #15**: Yes! Exceeded 80% threshold for
public release.

Both lines describe 2026-04-24 and are not a current assessment. The Security
row they include has since been overtaken by the live settings noted in the
preceding paragraph, so the real figure today is lower. Re-score against the live settings before
treating the 80% threshold as met.

---

## Next Steps

### Immediate (Before Public Release)

1. **Admin configures repository** - See ADMIN_SETUP.md (~45 minutes)
2. **Cut a release** - `[Unreleased]` carries entries, so v0.6.0 does not describe
  `main`. See RELEASING.md.
3. **Make repository public** - Settings → Danger Zone → Change visibility

### Post-Publication

1. **Monitor community health** - Track issue/PR response times
2. **Announce release** - Solace Community, social media, internal channels
3. **Submit to registries** - MCP server registry (if exists), Go package indexes

### Future Enhancements

- Add more badges (coverage, downloads, stars)
- Create GitHub Project board for roadmap visibility
- Set up GitHub Sponsors (if applicable)

---

## Contact

For questions about open source readiness:
- [support@solace.com](mailto:support@solace.com)
- See CONTRIBUTING.md for contribution process
- See SECURITY.md for security concerns

---

## References

Internal review documents (not committed):
- `Mark2-report.md` - Architecture review findings
- `open-source.md` - Open source readiness review with 11 findings

All findings from these reviews have been addressed in PR #15.
