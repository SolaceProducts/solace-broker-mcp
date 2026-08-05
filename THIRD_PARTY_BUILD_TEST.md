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

**Generated** 2026-08-05. Every licence below was read from the component's own
licence file or from the GitHub API for its source repository. None was inferred
from a package name or carried over from another row.

Kept honest by `.github/scripts/build-test-licenses-check.sh`, which fails CI
when this file stops matching what the repository actually uses. See
[Drift](#drift) for what it does and does not cover.

## Verdict

**Nothing here constrains how we license or distribute the product**, which is
the question this file exists to answer. Every Go module and every GitHub Action
is permissive (MIT, BSD-3-Clause, Apache-2.0).

Three qualifications, because the unqualified version of that sentence would be
wrong.

**Not everything here is open source.** The Claude Code CLI, used by the LLM eval
suite, ships under Anthropic's commercial terms rather than an OSI licence, and
the Solace broker image used as a test fixture is proprietary. Both are tools we
run, not code we link or redistribute, so neither affects our licensing position.
They are listed because the checklist asks for products *used*, not for products
that happen to be permissively licensed.

**One entry does ship.** `gcr.io/distroless/static-debian12` is the runtime base
of the container image we publish. See [Scope](#scope).

**Image layers are out of scope.** The tables record the licence of the project
that publishes each image, not the licences of the OS packages inside it. A
statement that no strong-copyleft licence appears anywhere in the build path
would be unsupportable on that basis alone: `golang:1.25-alpine`, the builder
stage, carries busybox and apk-tools under GPL-2.0. Builder-stage contents are
not distributed — only the distroless runtime stage is — so this does not change
the conclusion, but the claim is scoped rather than asserted.

## Go modules

### The root module contributes nothing

`go list -deps -test ./...` on the root module resolves to exactly the same
external module set as `go list -deps ./cmd/server`. Every test dependency is
already compiled into the binary and already listed in `THIRD_PARTY_LICENSES.md`,
so there is nothing to add here.

That is a real finding rather than an empty section, and it is the reverse of the
assumption that caused SOL-152414: `testify`, `go-spew`, and `go-difflib` were
excluded from the release inventory as "test-only" when they in fact reach the
binary. The direction of the error was surprising then and is worth stating
plainly now.

### `test/e2e-basic-mcp/agent`

A standalone module. It builds the MCP client that drives the end-to-end suite.

| Component | Version | License | License text |
|---|---|---|---|
| `github.com/google/jsonschema-go` | v0.4.2 | MIT | [license](https://github.com/google/jsonschema-go/blob/v0.4.2/LICENSE) |
| `github.com/modelcontextprotocol/go-sdk` | v1.5.0 | Apache-2.0 | [license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.5.0/LICENSE) |
| `github.com/segmentio/asm` | v1.1.3 | MIT | [license](https://github.com/segmentio/asm/blob/v1.1.3/LICENSE) |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | [license](https://github.com/segmentio/encoding/blob/v0.5.4/LICENSE) |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | [license](https://github.com/yosida95/uritemplate/blob/v3.0.2/LICENSE) |
| `golang.org/x/oauth2` | v0.35.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/oauth2/+/master:LICENSE) |
| `golang.org/x/sys` | v0.41.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/sys/+/master:LICENSE) |

Two notes a reader should not have to discover themselves.

**`golang.org/x/oauth2` is pinned here at v0.35.0 and at v0.36.0 in the root
module.** Two versions of one module live in this repository. Both are
BSD-3-Clause, so there is no licensing consequence, but the divergence is real
and recorded rather than smoothed over. It is a separate module with its own
`go.mod`, so nothing forces them to agree.

**The MCP Go SDK is mid-relicence.** Its licence file states that the project is
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
Solace's own Go messaging API, consumed here as an ordinary third-party
dependency, and it is Apache-2.0.

## npm packages

`test/e2e-llm/package.json` pins the Claude Code CLI that drives the LLM eval
suite, and `.github/workflows/llm-eval.yml` installs it with `npm ci`. It is a
test tool the suite invokes as a subprocess: nothing here is imported, linked, or
redistributed.

**This is the one source in this file that is not open source.** The package
declares `SEE LICENSE IN README.md`, which is Anthropic's commercial terms, not
an OSI licence. It is recorded as such rather than being normalised to something
tidier.

| Component | Version | License | License text |
|---|---|---|---|
| `@anthropic-ai/claude-code` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-darwin-arm64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-darwin-x64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-arm64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-arm64-musl` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-x64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-linux-x64-musl` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-win32-arm64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |
| `@anthropic-ai/claude-code-win32-x64` | 2.1.181 | Anthropic Commercial Terms | [terms](https://www.anthropic.com/legal/commercial-terms) |

Eight of the nine are per-platform binary packages that npm resolves as optional
dependencies of the first. Only one platform is ever installed on a given runner,
but all nine are in `package-lock.json` and any of them can be, so all nine are
listed.

## GitHub Actions

Actions run on GitHub's runners during CI. They are not distributed with the
product and never enter the binary.

| Action | Version | License | License text |
|---|---|---|---|
| `actions/checkout` | v4 | MIT | [license](https://github.com/actions/checkout/blob/main/LICENSE) |
| `actions/download-artifact` | v4 | MIT | [license](https://github.com/actions/download-artifact/blob/main/LICENSE) |
| `actions/setup-go` | v5 | MIT | [license](https://github.com/actions/setup-go/blob/main/LICENSE) |
| `actions/setup-node` | v4 | MIT | [license](https://github.com/actions/setup-node/blob/main/LICENSE) |
| `actions/upload-artifact` | v4 | MIT | [license](https://github.com/actions/upload-artifact/blob/main/LICENSE) |
| `docker/build-push-action` | v6 | Apache-2.0 | [license](https://github.com/docker/build-push-action/blob/master/LICENSE) |
| `docker/login-action` | v3 | Apache-2.0 | [license](https://github.com/docker/login-action/blob/master/LICENSE) |
| `docker/metadata-action` | v5 | Apache-2.0 | [license](https://github.com/docker/metadata-action/blob/master/LICENSE) |
| `docker/setup-buildx-action` | v3 | Apache-2.0 | [license](https://github.com/docker/setup-buildx-action/blob/master/LICENSE) |
| `docker/setup-qemu-action` | v3 | Apache-2.0 | [license](https://github.com/docker/setup-qemu-action/blob/master/LICENSE) |
| `golangci/golangci-lint-action` | v7 | MIT | [license](https://github.com/golangci/golangci-lint-action/blob/master/LICENSE) |
| `softprops/action-gh-release` | v2 | MIT | [license](https://github.com/softprops/action-gh-release/blob/master/LICENSE) |

### Solace-internal reusable workflows

Not third-party. Listed so the inventory accounts for every `uses:` in the
repository rather than silently skipping the ones that did not fit the table.

| Workflow | Ref | Owner |
|---|---|---|
| `SolaceDev/solace-public-workflows/.github/workflows/sca-scan-and-guard.yaml` | `fc521b0` | Solace |
| `SolaceDev/re-workflows/.github/workflows/transition-pr-on-merge.yaml` | `release/v3` | Solace |

`re-workflows` is scheduled for removal under SOL-152855, because a public
repository cannot resolve a reusable workflow from an internal repository in
another organisation. Whichever of the two changes lands second drops this row.

## Container images

Pulled during the build or by the end-to-end suites. Only one of these reaches a
published artifact; see [Scope](#scope).

| Image | Tag | Used for | Upstream license |
|---|---|---|---|
| `golang` | `1.25-alpine` | `Dockerfile` builder stage | BSD-3-Clause (Go) |
| `gcr.io/distroless/static-debian12` | `nonroot` | `Dockerfile` runtime base | Apache-2.0 (distroless) |
| `solace/solace-pubsub-standard` | `latest` | Broker fixture for e2e suites | Solace, proprietary |
| `quay.io/keycloak/keycloak` | `26.2.5` | IdP fixture for the OAuth e2e suite | Apache-2.0 |
| `apache/kafka` | `3.7.0` | Broker fixture for e2e suites | Apache-2.0 |

"Upstream license" is the licence of the project that publishes the image. A
container image is a stack of filesystem layers, and the layers carry operating
system packages under their own licences, which is not the same question. That
distinction matters for exactly one row, below.

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

- **Licence names.** Detecting that a component relicensed between versions needs
  the network and a warm module cache. Same limitation as `licenses-check.sh`,
  documented there for the same reason. When you bump a version, open the licence
  link.
- **Image layers.** See [Scope](#scope).
