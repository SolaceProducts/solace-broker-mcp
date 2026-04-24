## Summary

<!-- Brief description of what this PR does (1-2 sentences) -->

Fixes #(issue number)

## Type of Change

<!-- Check all that apply -->

- [ ] Bug fix (non-breaking change which fixes an issue)
- [ ] New feature (non-breaking change which adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change)
- [ ] Documentation update
- [ ] Refactoring (no functional changes)
- [ ] Performance improvement
- [ ] Dependency update

## Motivation and Context

<!-- Why is this change needed? What problem does it solve? -->

## Changes Made

<!-- Detailed list of changes. Use bullets for clarity. -->

-
-
-

## Testing

<!-- How was this tested? What cases were covered? -->

**Test Environment:**
- Go version:
- OS:
- Broker version (if applicable):

**Tests Performed:**
- [ ] Unit tests added/updated
- [ ] Integration tests added/updated (if applicable)
- [ ] E2E tests added/updated (if applicable)
- [ ] Tested locally with `go test -race ./...`
- [ ] Tested with real Solace broker

**Test Coverage:**
<!-- Describe what scenarios were tested -->
-

## Breaking Changes

<!-- If this is a breaking change, describe the impact and migration path -->

**Impact:**

**Migration Guide:**

## Checklist

<!-- Ensure all items are completed before requesting review -->

### Code Quality
- [ ] Code follows the [coding standards](../.github/CONTRIBUTING.md#coding-standards)
- [ ] Self-review completed (checked for typos, logic errors, edge cases)
- [ ] Code is well-commented (explains "why", not "what")
- [ ] Complex logic has explanatory comments
- [ ] No commented-out code or debug statements left in

### Security
- [ ] **No credentials in code** (checked all files, commits, and logs)
- [ ] Followed [secure logging rules](../docs/secure-logging-rules.md)
- [ ] Credential-carrying types implement `slog.LogValuer`
- [ ] No security vulnerabilities introduced (checked with golangci-lint/gosec)

### Testing
- [ ] All tests pass locally: `go test -race ./...`
- [ ] No race conditions detected
- [ ] Edge cases covered by tests
- [ ] Error paths tested
- [ ] Test names clearly describe what is being tested

### Documentation
- [ ] README.md updated (if user-facing changes)
- [ ] docs/ updated (if architecture/design changes)
- [ ] godoc comments added/updated for exported functions
- [ ] CHANGELOG.md updated (if applicable)
- [ ] Examples updated (if applicable)

### Git & CI
- [ ] All commits are signed off (DCO: `git commit -s`)
- [ ] Commits have clear, descriptive messages
- [ ] Branch is up to date with main (rebased if needed)
- [ ] No merge conflicts
- [ ] CI checks passing (lint, build, test, E2E)

### Breaking Changes (if applicable)
- [ ] Breaking changes documented in commit message
- [ ] Breaking changes documented in PR description
- [ ] Migration guide provided
- [ ] Deprecated functionality marked with comments and deprecation notices

## Screenshots (if applicable)

<!-- Add screenshots for UI changes, CLI output, etc. -->

## Additional Notes

<!-- Any other context, concerns, or follow-up work needed -->

## Related Issues/PRs

<!-- Link to related issues, PRs, or discussions -->

- Related to #
- Depends on #
- Blocks #

---

**For Reviewers:**

**Focus Areas**: <!-- What should reviewers pay special attention to? -->

**Questions**: <!-- Any specific questions for reviewers? -->
