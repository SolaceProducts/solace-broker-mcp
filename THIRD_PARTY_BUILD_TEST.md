# Third-Party Components Used at Build and Test Time

This file lists third-party components that the project uses to **build and test
itself**, and which are **not compiled into the shipped binary**.

> **This is not the release inventory.** The components distributed with the
> product are listed in [`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md).
> That file carries the Apache-2.0 section 4(d) attribution obligation and ships
> inside the release tarball and the container image. This one does neither: it
> exists to satisfy the second list required by the Solace [Guidelines for Public
> Repositories and Public Artifacts](https://sol-jira.atlassian.net/wiki/spaces/CPD/pages/2841706565)
> Legal checklist, which asks for a "list of all used 3rd party products used at
> build and test time" alongside the release list.

**Generated** 2026-09-04; the GitHub Actions section was refreshed 2026-08-07
when Guardian enrollment re-pinned every action to a commit SHA, and again
2026-08-14 when the Dependabot `github-actions` group update moved the five
`solace-public-workflows` actions to a newer commit on the same branch. Every
license in the following tables was read from the component's own license file or
from the GitHub API for its source repository, at the ref in use rather than at
the default branch. None was inferred from a package name or carried over from
another row. See [Rebuilding This File](#rebuilding-this-file) for how to
regenerate it.

Kept honest by `.github/scripts/build-test-licenses-check.sh`, which fails CI
when this file stops matching what the repository actually uses. See
[Drift](#drift) for what it does and does not cover.

## Verdict

**Nothing here constrains how we license or distribute the product**, which is
the question this file exists to answer. Every Go module and every GitHub Action
is permissive (MIT, BSD-3-Clause, Apache-2.0).

Three qualifications, because the unqualified version of that sentence would be
wrong.

**Not everything here is open source.** The Claude Code CLI, used by the large
language model (LLM) eval suite, ships under commercial terms from Anthropic
rather than an OSI license, and the Solace broker image used as a test fixture
is proprietary. Both are tools we
run, not code we link or redistribute, so neither affects our licensing position.
They are listed because the checklist asks for products *used*, not for products
that happen to be permissively licensed.

**One entry does ship.** `gcr.io/distroless/static-debian12` is the runtime base
of the container image we publish. See [Scope](#scope).

**Image layers are out of scope.** The tables record the license of the project
that publishes each image, not the licenses of the OS packages inside it. A
statement that no strong-copyleft license appears anywhere in the build path
would be unsupportable on that basis alone: `golang:1.25-alpine`, the builder
stage, carries busybox and apk-tools under GPL-2.0. Builder-stage contents are
not distributed — only the distroless runtime stage is — so this does not change
the conclusion, but the claim is scoped rather than asserted.

## Go Modules

### Root Module History

Prior to SOL-152417, `go list -deps -test ./...` on the root module resolved to
exactly the same external module set as `go list -deps ./cmd/server`. Every
test dependency was already compiled into the binary and already listed in
`THIRD_PARTY_LICENSES.md`, so there was nothing to add here.

That was a real finding rather than an empty section, and it is the reverse of
the assumption that caused SOL-152414: `testify`, `go-spew`, and `go-difflib`
were excluded from the release inventory as "test-only" when they in fact
reach the binary. The direction of the error was surprising then and is worth
stating plainly now.

### `test/e2e-basic-mcp/agent`

A standalone module. It builds the Model Context Protocol (MCP) client that drives the end-to-end suite.

| Component | Version | License | License text |
|---|---|---|---|
| `github.com/google/jsonschema-go` | v0.4.3 | MIT | [license](https://github.com/google/jsonschema-go/blob/v0.4.3/LICENSE) |
| `github.com/modelcontextprotocol/go-sdk` | v1.7.0 | Apache-2.0 | [license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/LICENSE) |
| `github.com/segmentio/asm` | v1.1.3 | MIT | [license](https://github.com/segmentio/asm/blob/v1.1.3/LICENSE) |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | [license](https://github.com/segmentio/encoding/blob/v0.5.4/LICENSE) |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | [license](https://github.com/yosida95/uritemplate/blob/v3.0.2/LICENSE) |
| `golang.org/x/oauth2` | v0.35.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/oauth2/+/master:LICENSE) |
| `golang.org/x/sync` | v0.20.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/sync/+/master:LICENSE) |
| `golang.org/x/sys` | v0.41.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/sys/+/master:LICENSE) |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/time/+/master:LICENSE) |

Three notes a reader should not have to discover themselves.

**`golang.org/x/oauth2` is pinned here at v0.35.0 and at v0.36.0 in the root
module.** Two versions of one module live in this repository. Both are
BSD-3-Clause, so there is no licensing consequence, but the divergence is real
and recorded rather than smoothed over. It is a separate module with its own
`go.mod`, so nothing forces them to agree.

**`github.com/segmentio/asm` relicensed after the version we consume.** v1.1.3 is
MIT. Upstream moved to MIT-0 ("MIT No Attribution") in v1.2.1 on 2023-11-07, so
the default branch — and therefore a bare
`gh api repos/segmentio/asm --jq .license.spdx_id` — reports MIT-0. That query
answers for the default branch, never for the pinned tag. Read the license at the
tag instead: `gh api repos/OWNER/REPO/license?ref=<tag>` accepts a ref, and
returns MIT here. This row is worth the paragraph because the drift check compares
names and versions and never licenses, so a future bump past v1.2.0 changes the
correct value in this column without turning CI red.

**The MCP Go SDK is mid-relicensing.** Its license file states that the project is
transitioning from MIT to Apache-2.0, that contributions whose authors have
consented are Apache-2.0, and that contributions from authors who have not
consented remain MIT. Both are permissive and both are compatible with our use,
so the practical answer is unchanged. Recorded as Apache-2.0 to match
`THIRD_PARTY_LICENSES.md`, which lists the same module for the shipped binary.

### `test/e2e-common/broker-driver`

A standalone module. It drives broker fixtures for the end-to-end suites.

| Component | Version | License | License text |
|---|---|---|---|
| `solace.dev/go/messaging` | v1.10.1 | Apache-2.0 | [license](https://github.com/SolaceProducts/pubsubplus-go-client/blob/v1.10.1/LICENSE) |

The only module in this file that appears nowhere in the release inventory. It is
a Go messaging API that Solace publishes itself, consumed here as an ordinary
third-party dependency, and it is Apache-2.0.

## npm Packages

`test/e2e-llm/package.json` pins the Claude Code CLI that drives the LLM eval
suite, and `.github/workflows/llm-eval.yml` installs it with `npm ci`. It is a
test tool the suite invokes as a subprocess: nothing here is imported, linked, or
redistributed.

**This is the one source in this file that is not open source.** The package
declares `SEE LICENSE IN README.md`, which is commercial terms from Anthropic, not
an OSI license. It is recorded as such rather than being normalized to something
tidier.

| Component | Version | License | License text |
|---|---|---|---|
| `@anthropic-ai/claude-code` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-darwin-arm64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-darwin-x64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-arm64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-arm64-musl` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-x64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-x64-musl` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-win32-arm64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-win32-x64` | 2.1.231 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |

Eight of the nine are per-platform binary packages that npm resolves as optional
dependencies of the first. Only one platform is ever installed on a given runner,
but all nine are in `package-lock.json` and any of them can be, so all nine are
listed.

## GitHub Actions

Actions run on GitHub runners during CI. They are not distributed with the
product and never enter the binary.

Every action is pinned to a commit SHA rather than a moving tag, so the "Pinned
ref" column carries a short SHA and is what the drift check compares. "Release"
is the tag that commit corresponds to, recorded because a SHA alone tells a
reader nothing about which version they are auditing. License links resolve at
that tag, not at the default branch, for the reason the preceding `segmentio/asm`
note spells out.

| Action | Pinned ref | Release | License | License text |
|---|---|---|---|---|
| `actions/attest-build-provenance` | `4d10147` | v4.2.2 | MIT | [license](https://github.com/actions/attest-build-provenance/blob/v4.2.2/LICENSE) |
| `actions/checkout` | `3d3c42e` | v7.0.1 | MIT | [license](https://github.com/actions/checkout/blob/v7.0.1/LICENSE) |
| `actions/dependency-review-action` | `a1d282b` | v5.0.0 | MIT | [license](https://github.com/actions/dependency-review-action/blob/v5.0.0/LICENSE) |
| `actions/download-artifact` | `3e5f45b` | v8.0.1 | MIT | [license](https://github.com/actions/download-artifact/blob/v8.0.1/LICENSE) |
| `actions/github-script` | `3a2844b` | v9.0.0 | MIT | [license](https://github.com/actions/github-script/blob/v9.0.0/LICENSE.md) |
| `actions/setup-go` | `b7ad1da` | v7.0.0 | MIT | [license](https://github.com/actions/setup-go/blob/v7.0.0/LICENSE) |
| `actions/setup-node` | `8207627` | v7.0.0 | MIT | [license](https://github.com/actions/setup-node/blob/v7.0.0/LICENSE) |
| `actions/upload-artifact` | `043fb46` | v7.0.1 | MIT | [license](https://github.com/actions/upload-artifact/blob/v7.0.1/LICENSE) |
| `docker/build-push-action` | `53b7df9` | v7.3.0 | Apache-2.0 | [license](https://github.com/docker/build-push-action/blob/v7.3.0/LICENSE) |
| `docker/login-action` | `dbcb813` | v4.6.0 | Apache-2.0 | [license](https://github.com/docker/login-action/blob/v4.6.0/LICENSE) |
| `docker/metadata-action` | `dc80280` | v6.2.0 | Apache-2.0 | [license](https://github.com/docker/metadata-action/blob/v6.2.0/LICENSE) |
| `docker/setup-buildx-action` | `37fe631` | v4.3.0 | Apache-2.0 | [license](https://github.com/docker/setup-buildx-action/blob/v4.3.0/LICENSE) |
| `docker/setup-qemu-action` | `96fe6ef` | v4.2.0 | Apache-2.0 | [license](https://github.com/docker/setup-qemu-action/blob/v4.2.0/LICENSE) |
| `github/codeql-action/analyze` | `db488dd` | v4.37.8 | MIT | [license](https://github.com/github/codeql-action/blob/v4.37.8/LICENSE) |
| `github/codeql-action/init` | `db488dd` | v4.37.8 | MIT | [license](https://github.com/github/codeql-action/blob/v4.37.8/LICENSE) |
| `golangci/golangci-lint-action` | `ba0d7d2` | v9.3.0 | MIT | [license](https://github.com/golangci/golangci-lint-action/blob/v9.3.0/LICENSE) |
| `softprops/action-gh-release` | `3d0d988` | v3.0.2 | MIT | [license](https://github.com/softprops/action-gh-release/blob/v3.0.2/LICENSE) |

`actions/github-script` names its license file `LICENSE.md` rather than
`LICENSE`. Worth the sentence only because the obvious URL 404s, and a reader who
hits that is likely to assume the row is wrong rather than that the filename is
unusual.

### Solace-internal composite actions

Not third-party. Listed so the inventory accounts for every `uses:` in the
repository rather than silently skipping the ones that did not fit the table.
All five come from one repository, pinned to a single commit.

| Action | Ref | Owner |
|---|---|---|
| `SolaceDev/solace-public-workflows/.github/actions/fossa-guard` | `ba836c7` | Solace |
| `SolaceDev/solace-public-workflows/.github/actions/sca/sca-scan` | `ba836c7` | Solace |
| `SolaceDev/solace-public-workflows/guardian-db-sync` | `ba836c7` | Solace |
| `SolaceDev/solace-public-workflows/guardian-vulnerability-gate` | `ba836c7` | Solace |
| `SolaceDev/solace-public-workflows/prisma-cloud-scan` | `ba836c7` | Solace |

`ba836c7` is a branch commit, not a tag, so there is no release to record beside
it. The repository itself is Apache-2.0. Re-pinned 2026-09-03 by the Dependabot
`github-actions` group update (#373); the prior pin was `6c01e11`.

Two entries have been dropped from this table, both by the reverse-direction
check rather than by anyone remembering to look.

`SolaceDev/re-workflows/.github/workflows/transition-pr-on-merge.yaml` went when
SOL-152855 removed the workflow that called it: a public repository cannot
resolve a reusable workflow from an internal repository in another organization.

`SolaceDev/solace-public-workflows/.github/workflows/sca-scan-and-guard.yaml`
went when DATAGO-147232 moved FOSSA and Prisma scanning off the Vault-backed
reusable workflow onto the preceding composite actions. This table previously held
that one row; it now holds five, and the whole preceding third-party table changed
from tags to SHA pins in the same change. None of it was reflected here until
this update, which is what SOL-152951 is about.

## Container Images

Pulled during the build or by the end-to-end suites. Only one of these reaches a
published artifact; see [Scope](#scope).

| Image | Tag | Used for | Upstream license |
|---|---|---|---|
| `golang` | `1.25-alpine` | `Dockerfile` builder stage | BSD-3-Clause (Go) |
| `gcr.io/distroless/static-debian12` | `nonroot` | `Dockerfile` runtime base | Apache-2.0 (distroless) |
| `solace/solace-pubsub-standard` | `latest` | Broker fixture for e2e suites | Solace, proprietary |
| `quay.io/keycloak/keycloak` | `26.2.5` | identity provider (IdP) fixture for the OAuth e2e suite | Apache-2.0 |
| `apache/kafka` | `3.7.0` | Broker fixture for e2e suites | Apache-2.0 |

"Upstream license" is the license of the project that publishes the image. A
container image is a stack of filesystem layers, and the layers carry operating
system packages under their own licenses, which is not the same question. That
distinction matters for exactly one row, addressed in the following section.

## Scope

Two boundaries, stated so that a reader does not mistake this file for covering
more than it does.

**`gcr.io/distroless/static-debian12` is the only entry here that ships.** It is
the runtime base of the container image we publish, so its layers are distributed
to users. `THIRD_PARTY_LICENSES.md` covers the Go modules linked into the binary
and does not describe the base image's OS packages, so neither file currently
enumerates them. Distroless `static` is deliberately minimal — no shell, no
package manager, essentially CA certificates, timezone data, and a passwd file —
which is why this has not been urgent. It is a genuine gap in the release
inventory rather than in this one, and it belongs to whoever picks up the
container-image side of the compliance artifacts.

**Broker and IdP fixture images are test infrastructure.** They run beside the
tests, are never linked, and are never redistributed by us.

## Rebuilding This File

**`make refresh-third-party-inventory`** does this and the equivalent for
`THIRD_PARTY_LICENSES.md` automatically: it diffs against
`build-test-licenses-check.sh`'s own verdict, rewrites only the row(s) that
actually drifted (reading each new license fresh, never inferring one), and
refuses outright rather than guess at anything it doesn't recognize —
review its diff and commit as usual. A Dependabot PR still needs a human to
run it; Dependabot itself cannot execute post-update scripts (SOL-152956).

The following commands are what that target runs under the hood, kept here for
anyone auditing the inventory by hand rather than trusting the script.
These four commands print what the preceding tables must contain, one per source.
`.github/scripts/build-test-licenses-check.sh` derives the same sets and points
here when it fails, so this is the single copy of the procedure.

```bash
# Go modules — every external module in every test submodule's closure.
# The grep drops this repository's own module paths, including each submodule's,
# which are not third-party components. The check applies the same exclusion.
find test -name go.mod -exec dirname {} \; | while read -r m; do \
    (cd "$m" && go list -deps -test -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./...); done \
    | grep -v '^github\.com/SolaceProducts/solace-broker-mcp' | sort -u

# npm packages
find test -name package-lock.json -exec jq -r \
    '.packages | to_entries[] | select(.key != "") | "\(.key | sub("^node_modules/"; "")) \(.value.version)"' {} \; | sort -u

# GitHub Actions and reusable workflows.
# The `grep -v '^\./'` drops this repository's own workflows, called as
# `uses: ./.github/workflows/...`. They are not third-party and have no row.
grep -rhoE '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]+' .github/workflows/ \
    | sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//' | grep -v '^\./' | sort -u

# Container images. The prune is deliberate, see below. The `-o` on grep drops the
# `AS <stage>` alias, matching how the check reads a FROM line.
find -L . \( -name .git -o -name .claude -o -name .worktrees -o -name node_modules \) -prune \
    -o -name 'Dockerfile*' -type f -exec grep -hoE '^FROM[[:space:]]+[^[:space:]]+' {} \; \
    | sed -E 's/^FROM[[:space:]]+//'
grep -rhoE '^[[:space:]]*image:[[:space:]]*[^[:space:]]+' test/ .github/workflows/ \
    | sed -E 's/^[[:space:]]*image:[[:space:]]*//'
```

Build-stage aliases, `scratch`, and images we publish ourselves are not
components and have no rows.

Two things the commands cannot do for you.

**Licenses are read, never inferred.** Open the component's own LICENSE file, or
run `gh api repos/OWNER/REPO --jq .license.spdx_id` for an action. Do not copy a
license from a neighboring row and do not guess it from a package name. The
check enforces names and versions but never licenses — see [Drift](#drift) — so
this rule is the only thing keeping that column true.

**The prune is deliberate.** `find` follows symlinks here, so without `-prune` it
descends into `.claude/worktrees/` and `.worktrees/` and reports another branch's
`Dockerfile` as a component of this one.

## Drift

`.github/scripts/build-test-licenses-check.sh` runs in CI on every pull request
and at release, alongside the check that guards `THIRD_PARTY_LICENSES.md`.

What it enforces, in both directions, so a row that outlives its component fails
just as loudly as a component with no row:

- Every external Go module in every test submodule's `go list -deps -test`
  closure, at the version in use.
- Every npm package in every `package-lock.json` under `test/`, at the version in
  use.
- Every `uses:` reference in `.github/workflows/`, at the ref in use. A 40-character
  SHA pin is satisfied by a short-SHA row, so the tables stay readable without the
  check going blind to a re-pin.
- Every `FROM` and every compose `image:`, at the tag in use.

**Discovery is derived, not listed.** Submodules, lockfiles, and Dockerfiles are
found with `find`, so adding a third submodule or a second Dockerfile cannot
introduce undocumented components that a hand-maintained list would miss. That is
the property that makes this gate fail closed as the repository grows, and it is
worth preserving over any convenience.

What it does not enforce:

- **License names.** Detecting that a component relicensed between versions needs
  the network and a warm module cache. Same limitation as `licenses-check.sh`,
  documented there for the same reason. When you bump a version, open the license
  link.
- **Image layers.** See [Scope](#scope).
