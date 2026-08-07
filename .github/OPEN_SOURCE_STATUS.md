# Open Source Readiness Status

**Last Updated**: 2026-08-07
**Originating PR**: #15 (aross/SOL-149029-open-source-compliance), merged
**Last revised by**: SOL-152412 (reconciled against the live repository settings)

This repository is public. What follows is partly a historical record of the
open-source readiness work (PR #15, clearly marked where it is historical) and
partly a pointer to `.github/ADMIN_SETUP.md`, which is the current source of truth
for how the repository is configured and what is outstanding. Where the two
disagree, ADMIN_SETUP.md wins — it is re-read against the API.

---

## Completion Status

### ✅ CRITICAL (All Complete)

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
  - GitHub Discussions is **not** enabled. Support runs on two channels:
    GitHub Issues for bugs and features, and a dedicated Solace Community
    (Discourse) category for questions and discussion. Vulnerabilities go through
    private disclosure per SECURITY.md.
  - This supersedes the earlier plan in this file to open Discussions.
  - No file links to `/discussions` any more; PR #272 (SOL-152946) repointed them
    at `https://solace.community/`. Remaining work: stand up the project's own
    Solace Community category and point the links at it — SOL-152413.
- [x] **M3: README Badges** - Already added in first commit (build, license, Go version, CoC)

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
✅ **Templates work** - GitHub automatically recognizes them
✅ **SECURITY.md visible** - GitHub shows in Security tab
✅ **COC and Contributing** - GitHub shows in sidebar
✅ **PR template active** - Applies to all new PRs
✅ **Issue templates active** - Applies to all new issues

## What Requires Admin

These are the settings only a repository admin can touch. Each one now has a
section in `.github/ADMIN_SETUP.md` recording its live value and the reasoning;
this list is a map to those sections, not a second copy of the state.

### 1. Repository Metadata
- Description and website URL
- Topics for discoverability
- See: ADMIN_SETUP.md → "Repository Settings"

### 2. Support channels
- GitHub Discussions stays **off**; Issues is on
- See: ADMIN_SETUP.md → "Features"

### 3. Branch Protection
- The `main-protection` ruleset requires a Code Owners approval, resolved
  conversation threads, and 13 status checks; it blocks force pushes and deletions
- **`Guardian scan gate`** (from `guardian-scan.yaml` job `gate`) is the
  supply-chain context. It is the *only* safe one to require: it always reports.
  Neither `Guardian scan` — which is the workflow's name and has never been a check
  at all — nor `Guardian gate`, which is skipped on every pull request, belongs in
  the list. ADMIN_SETUP.md explains both failure modes; they are easy to get wrong
  and one of them blocks all merges
- **`DCO sign-off` and `DCO check self-test`** are both required. DCO is the
  control this project carries in place of a contributor licence agreement, and an
  unrequired check enforces nothing
- See: ADMIN_SETUP.md → "Branch Protection" for the authoritative 13-context list

### 4. Security Features
- Secret scanning and secret-scanning push protection are on, as are validity
  checks, non-provider patterns, and AI detection. Confirm, do not re-enable
- Dependabot alerts and Dependabot security updates are on. Scheduled Go module and
  GitHub Actions updates run on Dependabot too (`.github/dependabot.yml`) —
  Renovate can't be enrolled for a public repo under the org's current system, so
  it stays scoped to just the pinned Claude Code CLI version
  (`.github/renovate.json`). Dependabot's PRs are exempted from the `DCO sign-off`
  check rather than expected to satisfy it; see `.github/workflows/dco.yaml`
- CodeQL runs on every PR but is **not** a required check, so a finding does not
  block a merge, and the repository API reports Code Security as disabled so we
  cannot say which configuration owns it. Outstanding under SOL-152858
- Private vulnerability reporting is **not** enabled. Outstanding under SOL-152967
- See: ADMIN_SETUP.md → "Security Settings"

### 5. Actions Settings
- "Allow GitHub Actions to create and approve pull requests" is off, and the
  default workflow token permission is read-only
- Fork pull request workflows should require approval for all outside
  collaborators. **No REST API exposes this setting**, so it cannot be verified by
  tooling — someone with org-admin access has to read the radio button.
  Outstanding under SOL-152960
- See: ADMIN_SETUP.md → "GitHub Actions Permissions"

### 6. Releases
- The newest release is v0.6.0 (tagged 2026-07-28) and `main` has moved a long way
  past it, with `[Unreleased]` entries in `CHANGELOG.md` including two BREAKING
  ones. v0.6.0 does **not** describe `main`, so someone downloading the latest
  release gets something behind the code they are reading. Cut one
- See: ADMIN_SETUP.md → "Releases"

---

## Open Source Maturity Progress

This table is a historical record of PR #15 (2026-04-24), not current state. The
Security row in particular has been overtaken in both directions since: secret
scanning and push protection were enabled, "Allow GitHub Actions to create and
approve pull requests" was turned off, and the required-status-check list was
corrected — while CodeQL enforcement and private vulnerability reporting remain
open. Do not cite the score below as evidence of anything; re-read the live
settings through `.github/ADMIN_SETUP.md`.

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

Both lines describe 2026-04-24 and are not a current assessment. Re-score against
the live settings rather than quoting them.

---

## Next Steps

### Open

1. **Close the outstanding admin items** - the authoritative list is the
   "Outstanding actions" table at the top of `.github/ADMIN_SETUP.md`
2. **Cut a release** - `[Unreleased]` carries entries, including two BREAKING ones,
   so v0.6.0 does not describe `main`. See RELEASING.md.

### Ongoing

1. **Monitor community health** - Track issue/PR response times
2. **Announce releases** - Solace Community, social media, internal channels
3. **Submit to registries** - MCP server registry (if exists), Go package indexes

### Future Enhancements

- Add more badges (coverage, downloads, stars)
- Create GitHub Project board for roadmap visibility
- Set up GitHub Sponsors (if applicable)

---

## Contact

For questions about open source readiness:
- support@solace.com
- See CONTRIBUTING.md for contribution process
- See SECURITY.md for security concerns

---

## References

Internal review documents (not committed):
- `Mark2-report.md` - Architecture review findings
- `open-source.md` - Open source readiness review with 11 findings

All findings from these reviews have been addressed in PR #15.
