# Release Guide

This document describes how to create a new release of the Solace Broker MCP Server.

## Prerequisites

- [ ] All PRs for the release are merged to `main`
- [ ] CHANGELOG.md is updated with all changes in `[Unreleased]`
- [ ] All CI checks passing on `main`
- [ ] You have push access to the repository

## Release Process

### 1. Prepare the Release

**Switch to main and ensure it's up to date:**
```bash
git checkout main
git pull origin main
```

**Verify CI status:**
```bash
gh run list --branch main --limit 1
```

Ensure the latest run shows all checks passing (build, lint, e2e, e2e-oauth).

### 2. Update CHANGELOG.md

**Move unreleased changes to new version section:**

Edit `CHANGELOG.md`:
```markdown
## [Unreleased]

<!-- Empty for now - add items as new PRs merge -->

## [0.1.0] - 2026-04-24

### Added
- (move all items from Unreleased section here)
```

**Update comparison links at bottom:**
```markdown
- [Unreleased]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.1.0...HEAD
- [0.1.0]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.1.0
```

**Commit the CHANGELOG update:**
```bash
git add CHANGELOG.md
git commit -s -m "Prepare CHANGELOG for v0.1.0 release"
git push origin main
```

### 3. Create Git Tag

**Create an annotated tag:**
```bash
VERSION=v0.1.0
git tag -a $VERSION -m "Release $VERSION

Initial public release of Solace Broker MCP Server

Highlights:
- Apache 2.0 license with full open source compliance
- YAML-driven composite tool engine
- OAuth/JWT authentication
- Production deployment support (Docker, Kubernetes)
- Comprehensive E2E test suite

See CHANGELOG.md for full details."
```

**Push the tag:**
```bash
git push origin $VERSION
```

This will automatically trigger the `.github/workflows/release.yml` workflow (added in PR #14) which:
- Builds binaries for Linux, macOS (amd64/arm64), Windows
- Generates SHA256 checksums
- Creates a GitHub Release with binaries attached

### 4. Monitor Release Workflow

**Check workflow status:**
```bash
gh run list --workflow=release.yml --limit 1
```

**View workflow details:**
```bash
gh run view --log
```

**Expected output**: Archives created in ~2-3 minutes
- `solace-broker-mcp-{version}-linux-amd64.tar.gz`
- `solace-broker-mcp-{version}-linux-arm64.tar.gz`
- `solace-broker-mcp-{version}-darwin-amd64.tar.gz`
- `solace-broker-mcp-{version}-darwin-arm64.tar.gz`
- `checksums-sha256.txt`

### 5. Verify GitHub Release

**View the release:**
```bash
gh release view $VERSION
```

**Check release page in browser:**
```
https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.1.0
```

**Verify:**
- [ ] Release title is correct ("v0.1.0")
- [ ] Release notes are auto-generated from merged PRs
- [ ] All 4 archives are attached
- [ ] `checksums-sha256.txt` is attached
- [ ] "Latest" badge is applied

### 6. Test the Release Binaries

**Download and test a binary:**
```bash
# macOS arm64 example
curl -LO https://github.com/SolaceDev/solace-broker-mcp/releases/download/v0.1.0/solace-broker-mcp-v0.1.0-darwin-arm64.tar.gz
tar xzf solace-broker-mcp-v0.1.0-darwin-arm64.tar.gz
./solace-broker-mcp -version
# Should output: v0.1.0
```

**Verify checksum:**
```bash
curl -LO https://github.com/SolaceDev/solace-broker-mcp/releases/download/v0.1.0/checksums-sha256.txt
shasum -a 256 -c checksums-sha256.txt --ignore-missing
# Should output: solace-broker-mcp-v0.1.0-darwin-arm64.tar.gz: OK
```

### 7. Update README (Optional Enhancement)

Add installation instructions to README.md:

```markdown
## Installation

### Download Pre-built Binary

Download the latest release from [GitHub Releases](https://github.com/SolaceDev/solace-broker-mcp/releases/latest):

**Linux (amd64):**
```bash
curl -LO https://github.com/SolaceDev/solace-broker-mcp/releases/latest/download/solace-broker-mcp-linux-amd64
chmod +x solace-broker-mcp-linux-amd64
sudo mv solace-broker-mcp-linux-amd64 /usr/local/bin/solace-broker-mcp
```

**macOS (arm64/Apple Silicon):**
```bash
curl -LO https://github.com/SolaceDev/solace-broker-mcp/releases/latest/download/solace-broker-mcp-darwin-arm64
chmod +x solace-broker-mcp-darwin-arm64
sudo mv solace-broker-mcp-darwin-arm64 /usr/local/bin/solace-broker-mcp
```

**macOS (amd64/Intel):**
```bash
curl -LO https://github.com/SolaceDev/solace-broker-mcp/releases/latest/download/solace-broker-mcp-darwin-amd64
chmod +x solace-broker-mcp-darwin-amd64
sudo mv solace-broker-mcp-darwin-amd64 /usr/local/bin/solace-broker-mcp
```

**Windows (amd64):**
Download `solace-broker-mcp-windows-amd64.exe` from the [releases page](https://github.com/SolaceDev/solace-broker-mcp/releases/latest).

**Verify installation:**
```bash
solace-broker-mcp -version
# Should output: v0.1.0
```

### Build from Source

See [Development Setup](#development-setup) section below.
```
```

### 8. Announce the Release

After the release is published:

**Update README.md badge** (if using dynamic latest badge):
- ✅ Already added in current PR: `[![Latest Release](https://img.shields.io/github/v/release/SolaceDev/solace-broker-mcp)](https://github.com/SolaceDev/solace-broker-mcp/releases/latest)`

**Announce in:**
- [ ] GitHub Discussions (Announcements category)
- [ ] Solace Community forum (https://solace.community/)
- [ ] Internal Solace channels (Slack, Teams, etc.)
- [ ] Social media (if applicable)

**Example announcement:**
```markdown
# Solace Broker MCP Server v0.1.0 Released!

We're excited to announce the first public release of the Solace Broker MCP Server - an MCP (Model Context Protocol) server that lets you manage Solace brokers using Claude Code and other AI agents.

**What's New:**
- Apache 2.0 licensed, fully open source
- YAML-driven composite tools for complex workflows
- OAuth/JWT authentication for production deployments
- Docker and Kubernetes deployment support
- Comprehensive E2E test suite

**Get Started:**
Download binaries from [GitHub Releases](https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.1.0)

**Learn More:**
- README: https://github.com/SolaceDev/solace-broker-mcp
- Architecture: https://github.com/SolaceDev/solace-broker-mcp/blob/main/docs/architecture.md
- Contributing: https://github.com/SolaceDev/solace-broker-mcp/blob/main/.github/CONTRIBUTING.md
```

---

## Release Cadence

**Pre-1.0 (current phase):**
- Release as needed when significant features are added
- Increment MINOR version (0.1.0 → 0.2.0 → 0.3.0)
- Patch versions for bug fixes only (0.1.0 → 0.1.1)

**Post-1.0:**
- MAJOR releases for breaking changes (1.0.0 → 2.0.0)
- MINOR releases for new features (1.0.0 → 1.1.0)
- PATCH releases for bug fixes (1.0.0 → 1.0.1)
- Aim for quarterly MINOR releases
- PATCH releases as needed for critical bugs/security

---

## Hotfix Process

For critical bugs or security issues that need immediate release:

1. Create hotfix branch from the affected version tag:
   ```bash
   git checkout -b hotfix/v0.1.1 v0.1.0
   ```

2. Apply the fix and commit:
   ```bash
   git commit -s -m "Fix critical bug in XYZ"
   ```

3. Update CHANGELOG.md with patch version:
   ```markdown
   ## [0.1.1] - 2026-04-25

   ### Fixed
   - [SECURITY] Fixed credential leak in log output (#123)
   ```

4. Create tag and push:
   ```bash
   git tag -a v0.1.1 -m "Hotfix release v0.1.1"
   git push origin v0.1.1
   ```

5. Merge hotfix back to main:
   ```bash
   git checkout main
   git merge --no-ff hotfix/v0.1.1
   git push origin main
   ```

---

## Troubleshooting

### Release workflow fails

**Check workflow logs:**
```bash
gh run list --workflow=release.yml --limit 1
gh run view <run-id> --log
```

**Common issues:**
- Missing `GITHUB_TOKEN` permissions - Check Settings → Actions → Workflow permissions
- Build fails on specific platform - Check cross-compilation syntax
- Tag already exists - Delete and recreate: `git tag -d v0.1.0 && git push origin :refs/tags/v0.1.0`

### Release not marked as "latest"

Manually edit the release on GitHub:
1. Go to Releases page
2. Click "Edit" on the release
3. Check "Set as the latest release"
4. Save

### Missing binaries in release

Re-run the workflow:
```bash
gh run list --workflow=release.yml --limit 1
gh run rerun <run-id>
```

---

## First Release (v0.1.0) - Special Instructions

Since this is the first public release, follow these steps **after** merging all open source compliance PRs:

**Wait for:**
- [ ] PR #15 (open source compliance) merged
- [ ] Any other pending PRs merged
- [ ] `main` branch CI is green

**Then:**
1. Follow steps 1-6 above
2. In the GitHub Release, edit the auto-generated notes and prepend:
   ```markdown
   # 🎉 Initial Public Release

   This is the first public release of the Solace Broker MCP Server under Apache 2.0 license.

   ## Highlights

   - Complete MCP server implementation for Solace broker management
   - Production-ready with OAuth/JWT authentication
   - Docker and Kubernetes deployment support
   - Comprehensive testing and CI/CD

   See CHANGELOG.md for full details.

   ---

   (auto-generated notes below)
   ```

3. Check "Set as the latest release"
4. Publish!

---

## Automation Improvements (Future)

Consider these enhancements to the release process:

- **Automated CHANGELOG**: Use `git-chglog` or `conventional-changelog` to auto-generate from commits
- **Release notes**: Parse CHANGELOG.md and include in GitHub Release description
- **Pre-release validation**: Automated checks before tagging (no uncommitted changes, CI green, etc.)
- **Docker image publishing**: Push to GitHub Container Registry or Docker Hub on release
- **Homebrew formula**: Auto-update Homebrew tap on release (if we create one)
