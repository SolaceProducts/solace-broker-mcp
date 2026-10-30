# Copilot code review instructions

## Third-party inventory on Dependabot PRs

On a pull request opened by Dependabot, do not comment that
`THIRD_PARTY_LICENSES.md` or `THIRD_PARTY_BUILD_TEST.md` is out of date with
the bump. A scheduled workflow (SOL-153374) runs
`make refresh-third-party-inventory` on the PR branch and pushes the refreshed
files, and the required `Third-party licenses current` check blocks the merge
until they match. A comment opens a review thread that someone then has to
resolve by hand before the PR can merge.

On any other pull request, keep flagging a dependency change that leaves those
files out of date.
