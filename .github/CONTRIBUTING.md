# Contributing to Solace Broker MCP Server

Thank you for your interest in contributing to the Solace Broker MCP Server! This document provides guidelines for contributing to this project.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [How Can I Contribute?](#how-can-i-contribute)
  - [Reporting Bugs](#reporting-bugs)
  - [Suggesting Features](#suggesting-features)
  - [Contributing Code](#contributing-code)
- [Development Setup](#development-setup)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Testing Requirements](#testing-requirements)
- [Developer Certificate of Origin](#developer-certificate-of-origin)

## Code of Conduct

This project adheres to the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code. Please report unacceptable behavior to [andrea.ross@solace.com](mailto:andrea.ross@solace.com).

## How Can I Contribute?

### Reporting Bugs

Before creating a bug report, please check the [existing issues](https://github.com/SolaceProducts/solace-broker-mcp/issues) to avoid duplicates.

When reporting a bug, please include:

- **Clear, descriptive title** — Use a specific title like "SEMP client panics on nil response" instead of "It crashes"
- **Steps to reproduce** — Detailed steps that reliably trigger the bug
- **Expected behavior** — What you expected to happen
- **Actual behavior** — What actually happened
- **Environment details**:
  - Server version (`./server -version`)
  - Go version (`go version`)
  - Operating system (Linux, macOS, Windows)
  - Solace broker version
- **Configuration** — Your `broker-config.yaml` (redact credentials!)
- **Logs** — Relevant log output with structured fields (redact credentials!)
- **Screenshots** — If applicable

**Security vulnerabilities:** Do NOT report security issues via public GitHub issues. See [SECURITY.md](../SECURITY.md) for the responsible disclosure process.

### Suggesting Features

We welcome feature requests! Before submitting:

1. Check [existing issues](https://github.com/SolaceProducts/solace-broker-mcp/issues?q=label%3Aenhancement) and [discussions](https://github.com/SolaceProducts/solace-broker-mcp/discussions) for similar ideas
2. Consider if the feature aligns with the project's goals (SEMP API management via MCP)
3. Think about how it would benefit the broader community

When suggesting a feature, please include:

- **Problem statement** — What problem does this solve? Who is affected?
- **Proposed solution** — How should this work? Include examples if possible
- **Alternatives considered** — What other approaches did you think about?
- **Implementation notes** — Any technical constraints or considerations

### Contributing Code

We love pull requests! Here's the workflow:

1. **Discuss first** — For large changes, open an issue or discussion first to align on approach
2. **Fork and clone** — Fork the repo and clone your fork locally
3. **Create a branch** — Branch from `main`: `git checkout -b feature/your-feature`
4. **Make changes** — Follow our [coding standards](#coding-standards)
5. **Add tests** — PRs without tests will not be merged
6. **Run the full test suite** — `go test -race ./...` must pass
7. **Run linters** — `golangci-lint run` must pass with no errors
8. **Update documentation** — Update README.md, godoc comments, or docs/ as needed
9. **Sign your commits** — All commits must be signed off with [DCO](#developer-certificate-of-origin)
10. **Push and open a PR** — Push to your fork and open a PR against `main`

## Development Setup

See the [README.md Development Setup](../README.md#development-setup) section for detailed instructions.

Quick start:

```bash
# Clone your fork
git clone https://github.com/YOUR-USERNAME/solace-broker-mcp.git
cd solace-broker-mcp

# Install dependencies
go mod download

# Install the git hooks (adds the DCO sign-off) — once per clone
make hooks

# Create local config (see README for details)
cp broker-config.example.yaml broker-config.yaml
# Edit broker-config.yaml with your broker details

# Run tests
go test -race ./...

# Run linter
golangci-lint run

# Build and run
go run ./cmd/server
```

## Pull Request Process

### Before Submitting

- [ ] Code follows the [coding standards](#coding-standards)
- [ ] All tests pass: `go test -race ./...`
- [ ] Linter passes: `golangci-lint run`
- [ ] Documentation updated (README, godoc, docs/)
- [ ] Commits are signed off (DCO)
- [ ] PR description follows the template

### Getting CI to run on a branch

**Pushing a branch no longer runs CI on its own.** `build-and-test.yml` triggers on
pull requests against `main` and on pushes to `main`, not on every branch push.
Three ways to get a verdict, cheapest first:

1. **Locally**, which is the fastest loop and needs no CI at all:

   ```bash
   make check          # build, vet, lint, race tests
   make e2e-all        # the basic-mcp E2E suite against Dockerized brokers
   ```

2. **Run the full suite on your branch without a pull request**, via
   `workflow_dispatch`:

   ```bash
   gh workflow run build-and-test.yml --ref my-branch
   gh run watch        # or: gh run list --workflow build-and-test.yml
   ```

   This needs write access, so it is for maintainers rather than fork
   contributors.

3. **Open the pull request.** A draft is enough; `opened` and `synchronize` both
   fire for drafts, so every check runs and keeps running as you push.

Why it changed: those seven jobs are required status checks, and a check that never
reports leaves a required context pending forever. That is how a fork contribution
used to become unmergeable, because a fork's push fires in the fork and never
against this repository's commit. Moving to `pull_request` fixes that, and keeping
`push: '**'` alongside it would run every check twice on same-repo pull requests,
including five Dockerized E2E suites. The header comment in
`.github/workflows/build-and-test.yml` records the alternatives that were
considered and why they lose.

### PR Review Process

1. **Automated checks** — CI must pass (build, lint, test, E2E, DCO sign-off)
2. **Maintainer review** — At least one maintainer approval required
3. **Address feedback** — Respond to review comments and make requested changes
4. **Squash or rebase** — Clean up commit history if requested
5. **Maintainer merge** — A maintainer will merge when approved

**Response time:** We aim to provide initial feedback within 48 hours (business days). Larger PRs may take longer to review.

## Coding Standards

This project follows standard Go coding principles. Key guidelines:

### Go Style

- Follow [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- Use `gofmt` for formatting (enforced by CI)
- Use `golangci-lint` for linting (config: `.golangci.yml`)
- Exported functions must have godoc comments

### Naming Conventions

- **Package names:** Short, lowercase, singular (e.g., `config`, `semp`, `tools`)
- **Variables:** Descriptive, camelCase (e.g., `brokerPool`, `httpClient`)
- **Functions:** Verb-first, descriptive (e.g., `GetSempV2`, `ValidateParams`)
- **Interfaces:** `-er` suffix when applicable (e.g., `Client`, `Handler`)

### Error Handling

- Always check errors — do not use `_` to discard
- Wrap errors with context: `fmt.Errorf("reading config: %w", err)`
- Return errors; do not log and continue (except in deferred cleanup)
- Use structured errors for API errors (e.g., `sempv2.SEMPError`)

### Security — Credential Handling

**CRITICAL:** Never log credentials. See `docs/secure-logging-rules.md` for full details.

- Implement `slog.LogValuer` on types containing credentials (see `AuthConfig` example)
- Never use `slog.Any()` with credential-carrying types
- Always log explicit safe fields: `slog.String("url", cfg.URL)`
- Use `%q` format specifier for user input in errors

### Function Design

- Keep functions small (<50 lines when possible)
- Single responsibility — one function, one purpose
- Limit parameters (0-3 ideal, max 5)
- Use structs for configuration with many fields

### Comments

- Explain **why**, not **what** — code should be self-documenting
- Document assumptions, edge cases, and non-obvious decisions
- Use `//` for line comments, `/* */` for block comments
- Godoc comments start with the function/type name: `// GetSempV2 returns...`

## Testing Requirements

**All code changes must include tests.** PRs without tests will not be merged (except documentation-only changes).

### Test Types

1. **Unit tests** — Test individual functions in isolation
   - File naming: `*_test.go`
   - Use table-driven tests for multiple cases
   - Mock external dependencies (use `sempv2.Client` interface)

2. **Integration tests** — Test component interactions
   - Example: `internal/tools/manager_test.go`
   - Use real structs, mock only external APIs

3. **E2E tests** — Test full workflows against real Solace brokers
   - Location: `test/e2e-basic-mcp/`
   - Run via `bash test/e2e-basic-mcp/run-all.sh`
   - Only add when testing new user-facing features

### Running Tests

```bash
# All tests with race detector
go test -race ./...

# Specific package
go test -race ./internal/semp/...

# Verbose output
go test -v -race ./...

# E2E tests (requires Docker)
bash test/e2e-basic-mcp/run-all.sh
```

### Test Guidelines

- Tests must pass with `-race` flag (data race detection)
- No sleeps or timing dependencies — use channels/context for synchronization
- Clean up resources (close connections, remove temp files)
- Table-driven tests for validation logic (see `internal/config/config_test.go`)

## Developer Certificate of Origin

This project uses the [Developer Certificate of Origin (DCO)](https://developercertificate.org/) to ensure contributors have the right to submit their code.

By signing off your commits, you certify that:

1. You wrote the code yourself, OR
2. You have the right to submit it under the project's license (Apache 2.0), AND
3. You understand and agree that your contribution is public and may be redistributed under the Apache 2.0 license

### How to Sign Off Commits

Add the `-s` flag when committing:

```bash
git commit -s -m "Add feature X"
```

This adds a `Signed-off-by` line to your commit message:

```
Add feature X

Signed-off-by: Your Name <your.email@example.com>
```

#### Sign off automatically

To stop having to remember the `-s`, install the repo's git hook — once per clone:

```bash
make hooks
```

That installs `.githooks/prepare-commit-msg` into the repository's hooks directory
— `.git/hooks/` — where it adds the `Signed-off-by` trailer to every commit
message, including ones made by tools and editors that never pass `-s`. The copy
is read out of `origin/main`, **not** out of your working tree, so `git fetch`
before your first `make hooks` on a fresh clone. It signs off with the identity
git would record —
your `git config user.email`, or `GIT_COMMITTER_EMAIL` if you commit through
tooling that sets it — which is the same address CI matches against. It does not
duplicate the trailer if you also pass `-s`, and it never fails a commit.

Verify it works:

```bash
git commit --allow-empty -m "hook check"
git log -1 --format=%B     # expect a Signed-off-by line
git reset --soft HEAD~1    # drop the test commit, keep your working tree
```

The hook is a convenience, not the control: the `DCO sign-off` CI check is the
enforcement and re-verifies every commit regardless of what ran locally. Re-run
`make hooks` after a change to `.githooks/` lands on `main`; it upgrades its own
copy but refuses to overwrite an unrelated `prepare-commit-msg` hook you already
have, and refuses to install anywhere but inside this repository's git directory —
a `core.hooksPath` pointing into the working tree, or into a shared hooks directory
outside the repo, is a refusal, not a warning.

If you edit the hook, run its suite by hand — no CI job runs it:

```bash
.githooks/prepare-commit-msg.test.sh
```

To try your own edit, commit it and install from your branch explicitly:

```bash
HOOKS_REF=HEAD make hooks
```

`HEAD` is a commit, not your working tree, so an uncommitted edit installs nothing
new. That is the one case where you are opting into running your own branch; the
default `origin/main` is what keeps a branch you have checked out — a fork's pull
request, say — from installing itself into `.git/hooks/`, where it would outlive
the branch. Any ref works: `HOOKS_REF=upstream/main make hooks` if your remote is
not called `origin`.

Run it on the oldest git you expect contributors to have, not just yours. Two of
the cases pass on git 2.50 but catch a real bug only on git 2.39 — the version
Debian bookworm, Ubuntu 22.04 and RHEL 9 ship — so a green run on a current git
proves less than it looks like it does.

Those refusals, and the trusted ref, are all the same point: nothing about a
checked-out branch should decide what code runs on your machine during an ordinary
`git commit`. Activating the tracked directory with `core.hooksPath .githooks`, or
symlinking into it, would make git run the hook from whatever branch is checked out
— so reviewing a fork's pull request would execute that fork's hook. The full
reasoning is in the header of `.githooks/prepare-commit-msg`.

**All commits in a PR must be signed off.** If you forget, you can amend:

```bash
# Amend the last commit
git commit --amend -s --no-edit && git push --force-with-lease

# Sign off all commits in your branch (main = the branch you opened the PR against)
git rebase --signoff main && git push --force-with-lease
```

**If your branch contains a merge commit, do not rebase.** `git rebase` replays a
merge's parents as ordinary commits and throws the merge away, along with any
conflict resolution in it. Amend the merge if it is at the tip
(`git commit --amend -s --no-edit`), or re-create it with `git merge --signoff`.
The CI output tells you which case you are in.

`git commit -s` signs off with your `git config user.email`, which is also the
address CI matches against, so make sure it is set to the one you mean to use.

### Enforcement

The `DCO sign-off` check in CI blocks any PR that is missing a sign-off. What it
checks, precisely:

- Every commit your PR adds. Commits already on the branch you are targeting are
  not re-checked.
- Each of those commits needs a `Signed-off-by:` line, starting its own line,
  whose email matches that commit's own author or committer, compared
  case-insensitively. A sign-off copied along with someone else's commit does not
  count.
- Merge commits are exempt while re-merging their parents reproduces exactly what
  they recorded, so refreshing your branch with `git merge main` is fine. A merge
  that contributes something of its own — a conflict resolution, or files edited
  or added during the merge — needs its own sign-off (`git merge --signoff`).
  Octopus merges and merges the check cannot recompute always need one.
- `Co-Authored-By:` trailers are ignored. Co-authors do not need their own
  sign-off.

The check reports the offending commits and the exact commands to fix them. There
is no label or flag that skips it. Use your real name — no pseudonyms or
anonymous contributions.

If you are contributing from a fork, every one of our CI runs waits for a
maintainer to approve it, not just the one on your first push. So checks can sit
**pending**, and a check that was green can go back to pending after the PR is
retitled or edited. That is normal and nothing for you to fix.

You will also see the `FOSSA Scan` check report as **skipped**. Our licence and
vulnerability scan authenticates against an internal service, and GitHub correctly
withholds those credentials from a fork's workflow run, so the scan cannot run on
your pull request. The `SCA gate` check accounts for that and passes. If your
change adds or updates a dependency, expect a reviewer to look at `go.mod` and
`go.sum` closely, since the automated scan is not there to do it.

## Questions?

- **General questions:** Start a [GitHub Discussion](https://github.com/SolaceProducts/solace-broker-mcp/discussions)
- **Bugs or features:** Open a [GitHub Issue](https://github.com/SolaceProducts/solace-broker-mcp/issues/new/choose)
- **Security issues:** Email [andrea.ross@solace.com](mailto:andrea.ross@solace.com)
- **Community chat:** Visit [Solace Community](https://solace.community/)

## License

By contributing to this project, you agree that your contributions will be licensed under the [Apache License 2.0](../LICENSE).

---

Thank you for contributing to the Solace Broker MCP Server! 🎉
