# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Maintaining this file:** draft entries per PR under `[Unreleased]` (the
> `/changelog` skill drafts one from your diff), and the release process promotes
> `[Unreleased]` into a dated version block at tag time — see `RELEASING.md`. Entries
> for released versions below were reconstructed from the per-tag GitHub Release notes;
> the concise ones cite their SOL ticket, the detailed paragraphs were authored at the
> time of the change.

## [Unreleased]

### Fixed

- The token-exchange circuit breaker's `consecutive_failure_threshold` rule now trips a slow, low-traffic IdP outage, not only a fast-failing one. It previously read gobreaker's own `ConsecutiveFailures`, which decays as the rolling window's buckets (`failure_rate_window`/10) age out — failures spaced wider than a bucket period could leave the count permanently below threshold even though every exchange failed, and at traffic too low for `minimum_requests` the rate rule couldn't cover the gap either, so the breaker could stay closed through a genuine sustained outage. The rule now reads a separate, undecayed counter this package owns: it increments on every counted failure and resets only on an observed success (never on a timer, since Exchange sits behind a token cache — a quiet gap usually means no caller needed a token, not that the IdP recovered). Excluded outcomes (429, cancellations, config faults) still touch neither counter. No config or behavior change for the fast-failing case this rule always caught; `consecutive_failure_threshold: 0` still disables it. Tracked under SOL-152286.

## [0.7.1] - 2026-08-11

`v0.7.0` was tagged but its release pipeline failed before publishing anything, so it has no binary archives, no container image, and no GitHub Release — only a git tag and a Go module version. **0.7.1 is the first published release of the 0.7 line**, and because git history is cumulative it contains everything listed under 0.7.0 below in addition to the change here. Anyone looking for 0.7.0 downloads wants 0.7.1.

Taken together with 0.7.0, this is the first release published from the public repository, and the first whose artifacts can be verified: every binary archive and the container image now carries a build provenance attestation tracing it to the workflow run and commit that produced it, with the commands in `README.md`. Also new since 0.6.0 — the `describe-semp-schema` tool, which lets an agent enumerate a SEMPv2 operation's configurable attributes before attempting a write; an optional `filter_tools_list` flag narrowing `tools/list` to what the caller is actually authorized to invoke; and a second third-party inventory, `THIRD_PARTY_BUILD_TEST.md`, covering the components used to build and test the project rather than ship in it.

**Two breaking changes, both from the move to the `SolaceProducts` GitHub organization.** The Go module path is now `github.com/SolaceProducts/solace-broker-mcp`. The container image is published to `ghcr.io/solaceproducts/solace-broker-mcp` — and the old path does not fail loudly, it keeps serving the last image pushed there, so a stale reference silently stops receiving updates. Migration notes are in the **Changed** section under 0.7.0.

### Changed

- Updated the embedded SEMPv2 OpenAPI specs to the 10.26.3 rolling release (`10.26.3.10320`), so tool schemas track current broker attributes. The three spec files are renamed to `semp-v2-swagger-{action,config,monitor}.10.26.3.json` and continue to be sourced from the private-extended variant. Loading is unaffected — specs are picked up by the embed glob and typed from their `basePath`, not their filename. Tracked under SOL-152939.

## [0.7.0] - 2026-08-10

### Added

- New `THIRD_PARTY_BUILD_TEST.md` inventory covering the third-party components used to build and test the project but not shipped in the binary: the Go modules of the two e2e submodules, the nine npm packages the LLM eval suite installs, the GitHub Actions and reusable workflows CI uses, and the five container images pulled by the build and the e2e fixtures. This is the second of the two third-party lists the Solace public-repository Legal checklist requires; `THIRD_PARTY_LICENSES.md` remains the release inventory and is unchanged. Every licence was read from the component's own licence file or its source repository rather than inferred. Nothing found constrains how we license or distribute the product — every Go module and GitHub Action is permissive (MIT, BSD-3-Clause, Apache-2.0) — but the file deliberately does not claim universal permissiveness: the Claude Code CLI used by the LLM eval suite ships under Anthropic's commercial terms and the Solace broker test fixture is proprietary, both being tools we run rather than code we link or redistribute. Kept honest by `.github/scripts/build-test-licenses-check.sh`, which fails CI on drift in either direction across all four sources, including component *versions*, under its own self-test. Discovery is derived with `find` rather than hardcoded, and prunes rather than filters, so a new submodule, lockfile, or Dockerfile cannot introduce components the gate is blind to and a sibling worktree cannot inject ones it does not have. The rebuild procedure lives in the document itself rather than only on the gate's failure path. Two further facts surfaced and recorded rather than smoothed over: `golang.org/x/oauth2` is pinned at two different versions across the repository's modules (no licensing consequence, both BSD-3-Clause), and the distroless runtime base is the one build input that reaches a published artifact, so its OS-package layers are a gap in the *release* inventory that neither file currently enumerates. Tracked under SOL-152861.

- New MCP tool `describe-semp-schema` for retrieving the SEMPv2 schema slice of a given operation's request-body definition, so an agent planning a create or update can enumerate every configurable attribute before invoking the write tool. Read-only and broker-independent — registered standalone like `list-brokers`, with no `broker` parameter and no RBAC surface, since the response is derived from the embedded OpenAPI specs and reveals nothing broker-specific. Scoped to the config and action SEMPv2 APIs; the monitor API is read-only (GET-only, no request bodies), so its operations would only ever return an empty attribute list and are not indexed — a `monitor/...` operation returns `unknown operation`. Takes an `operation` identifier in `<specType>/<operationId>` form (e.g. `config/createMsgVpnQueue`) and an optional `view`: `trimmed` (default) returns a compact per-attribute list including type, description (first paragraph + enum `<pre>` block), enum values, default, `pattern`/`maxLength`/`minimum`/`maximum` constraints, writability flags (`writableOnCreate`/`writableOnUpdate`/`requiredForCreate`/`identifying`/`writeOnly`/`sensitive`/`deprecated`), `autoDisable` (attributes the broker temporarily disables when this field changes on a live object), and `requiresDisable`; object attributes backed by a `$ref` are resolved one level deep and their nested properties inlined rather than fabricating writability flags from absent extensions; `raw` returns the OpenAPI definition verbatim. Tracked under SOL-152603.

- Full live-broker E2E coverage (`test/e2e-monitoring`) for `list-kafka-receivers`, `get-kafka-receiver-status`, `list-kafka-senders`, and `get-kafka-sender-status` (added spec-derived in SOL-152328), closing the verification gap that story left open. A real single-node Kafka broker (`apache/kafka:3.7.0`, KRaft mode) was added to the suite's `docker-compose.yml` as the "kafka" service, matching how bridges bridge to a sibling Solace broker — both Solace brokers now bridge to it instead. New F9/F10 fixtures mirror bridges' three-state pattern (healthy, unreachable, admin-disabled) for both object types. This story initially concluded live verification was blocked by a Kafka-Bridging license/edition gate with no way around it; further investigation found that conclusion was wrong — creating a Kafka Receiver/Sender is gated by two enum-restricted scaling settings that default to 0 (`SYSTEM_SCALING_MAXKAFKABRIDGECOUNT`/`SYSTEM_SCALING_MAXKAFKABROKERCONNECTIONCOUNT` — not a license restriction), both now set in `docker-compose.yml`. With that unlocked, `failureReason` was confirmed to populate reliably (`"Shutdown"`, `"No remote-broker in UP state"`) — unlike bridges' `inboundFailureReason`, which never populates — and a healthy Kafka Receiver/Sender was confirmed to converge to `up: true` against a real external Kafka broker within seconds. Full findings, including the enum-valid scaling values and the binding-cascade-delete behavior, are in `test/e2e-monitoring/README.md`'s "F9/F10 — Kafka Receiver/Sender findings". Tracked under SOL-152370.

- New optional `mcp_client_auth.tool_authorization.filter_tools_list` flag narrows the `tools/list` response to the tools the caller's groups actually grant, instead of returning every registered tool to every caller. **Defaults to `false`** — absent behaves exactly as today, and with the flag off no middleware is installed, so the request path is unchanged. This is discovery hygiene, not an access control: `tools/call` remains the only authorization boundary and is enforced identically either way, and a tool absent from the list is still callable by name, since the server resolves tool calls against the full registered set rather than against whatever a previous list returned. The benefit is context — an agent handed tools it will be denied spends context on their descriptions and schemas and tends to misreport the cause when a call is refused; a caller in a narrow role can drop from ~24 tools to 2 or 3. The filter reuses the same policy decision as the call path, so a listed tool is always callable and a callable tool is always listed; the two structurally exempt tools (`list-brokers` and `describe-semp-schema`) are never filtered, since hiding a tool that stays callable would break that guarantee and would remove broker discovery for every caller. A caller whose groups match no grants receives a normal response containing only the exempt tools — never an error, never an empty list — and a token carrying no groups claim at all fails closed to the same response. Those two cases are deliberately indistinguishable to the caller but are separated in the server log: each filtered list emits one `msg: "tool list filter"` line tagged `event: "tool_list_filter"`, carrying the usual caller identity fields plus `decision_reason` (`filtered`, `unfiltered`, `not_permitted`, or `missing_claim`), `groups_present`, and `tools_before`/`tools_after` counts. `missing_claim` logs at `WARN` because the token could not answer the authorization question at all — typically an IdP claim-mapper misconfiguration affecting every caller of that deployment — while every other outcome logs at `INFO` as the policy working as configured; caller group names are never logged, matching the call path. Setting the flag while `tool_authorization.enabled` is `false` logs a startup `WARN` naming the reason and leaves filtering off rather than failing startup: there is no policy to filter against, so the state is inert rather than incorrect, and it is what an operator produces by flipping the master switch without touching anything else. The filtering posture is stated on every boot alongside the existing `tool authorization is enabled/disabled` line. Support remedy for a client that behaves oddly or a caller reporting missing tools: set `filter_tools_list: false` and restart — authorization stays fully enforced on every tool call. Tracked under SOL-152888.

- Release artifacts now carry build provenance attestations. The release workflow attaches a GitHub artifact attestation to each of the four binary archives and to the multi-arch container image, so a consumer can prove an artifact was produced by this repository's `release.yml` workflow rather than rebuilt or substituted elsewhere — a guarantee the existing `checksums-sha256.txt` cannot give, since it only proves a file matches the checksum list published beside it. Each archive is attested in the matrix job that built it, so its digest is bound before the archive round-trips through `upload-artifact`/`download-artifact`, and a failure to attest surfaces before anything is published. The image attestation is additionally pushed to the registry alongside the image, where `cosign` can verify it directly. Verify with the GitHub CLI: `gh attestation verify <archive> --repo SolaceProducts/solace-broker-mcp --signer-workflow SolaceProducts/solace-broker-mcp/.github/workflows/release.yml` for an archive, and the same flags against `oci://ghcr.io/solaceproducts/solace-broker-mcp:<version>` for the image (`<version>` has no leading `v` — the image tags are `1.2.0`, `1.2`, `latest`, `sha-<short-sha>`). `--signer-workflow` matters: `--repo` alone binds only the repository, so any workflow in it holding `id-token: write` and `attestations: write` could mint an attestation that passes. `gh` fetches the attestation from the GitHub API unless given `--bundle-from-oci`. Add `--source-digest <commit>` to bind the verification to a specific commit; `RELEASING.md` has the full runbook form. Signed *git tags* remain **[Planned]** — a separate mechanism, see `RELEASING.md`. Tracked under SOL-152543.

### Changed

- Every Go source file now carries the Apache-2.0 licence header, and a CI gate keeps it that way. 53 of 260 files had none: the headers were applied once by hand across a much smaller codebase, nothing checked them afterwards, and the codebase grew past them. No licensing consequence, since `LICENSE` at the repository root governs regardless, but Apache 2.0 works best when the grant travels with the file, and a downstream consumer who receives one `.go` file should be able to read its licence from that file rather than infer it. Comment-only change; no behavior, API, or tool-schema difference, confirmed by the diff being 742 pure insertions across 53 files (53 × 14 lines) with no deletions. The new `.github/scripts/license-header-check.sh` requires the first 13 lines of every `.go` file outside `vendor/` to be byte-identical to the canonical header and line 14 to be blank, and runs as the `Licence headers present` job on every pull request. Each of those three conditions closes a way the check could otherwise pass while the file is wrong: a `grep`-style search over whole files accepts a header pasted below the `package` clause, a substring match accepts a reworded one, and the blank line is Go semantics rather than formatting, because a comment block touching `package` becomes the package doc comment and would publish the licence text on pkg.go.dev in place of the real documentation. Its self-test pins all three, plus two ways the walk itself can go quietly blind: matching no files at all, and silently ceasing to descend into a subtree. The suite is kept honest by mutation rather than assertion count, each of those five defects having been applied to a copy of the script and confirmed to turn it red. The script also takes `--fix`, which prepends the header to files that have no licence text, updates the copyright line on files that differ from canonical in that line alone, and refuses anything else, so the next contributor does not hand-copy a fourteenth variant. That middle case is what keeps rolling the year range a one-line edit instead of 260 manual ones; the tolerance stops at a Solace copyright notice, since a third party's is not ours to rewrite. Tracked under SOL-152896.

- **BREAKING:** Go module path changed from `github.com/SolaceDev/solace-broker-mcp` to `github.com/SolaceProducts/solace-broker-mcp`, following the repository's move to the SolaceProducts GitHub org ahead of going public. Anyone importing this module needs to update their import path to match; the old path is no longer valid. No functional or behavioral change — every internal import was updated to match, and the two e2e test submodules' own module declarations were updated for consistency.
- Each of the eight `create-*`/`update-*` write tools (message-VPN, queue, topic endpoint, RDP) now carries a one-line pointer to the new `describe-semp-schema` tool with the SEMPv2 operation identifier the caller passes (e.g. `config/createMsgVpnQueue`), so an agent that needs the full attribute list beyond the ones inlined in the description knows where to find it. The `BEFORE INVOKING THIS TOOL` confirmation block was also lifted to the top of every write tool's description so the confirmation requirement is the first thing an agent reads. Description text only; no parameter or output-shape change. Tracked under SOL-152603.

- **BREAKING**: The published container image moved from `ghcr.io/solacedev/solace-broker-mcp` to `ghcr.io/solaceproducts/solace-broker-mcp`, matching the repository's move to the `SolaceProducts` organization ahead of going public. Releases from this version forward publish only to the new path; the old path stops receiving new tags, so a `docker pull`, Compose file, or Kubernetes manifest still pointing at `ghcr.io/solacedev/solace-broker-mcp` will keep resolving to the last image pushed there and silently stay behind. Migration: update every image reference to `ghcr.io/solaceproducts/solace-broker-mcp` — the tag scheme (`{version}`, `{major}.{minor}`, `latest`, `sha-<short-sha>`), image contents, config path, and health check are unchanged. Tracked under SOL-152543.

### Fixed

- `THIRD_PARTY_LICENSES.md` now matches the binary it describes, `NOTICE` propagates every dependency attribution it is obliged to, and both files now actually ship with the release. The inventory had drifted in both directions since it was generated on 2026-07-17: it listed `github.com/maypok86/otter` v1.2.4, `github.com/dolthub/maphash`, and `github.com/gammazero/deque`, none of which are in the binary any more, and omitted five components that are — `github.com/sony/gobreaker/v2` v2.4.0 (MIT), `github.com/maypok86/otter/v2` v2.3.0 (Apache-2.0), and, less obviously, `github.com/stretchr/testify` v1.11.1 (MIT), `github.com/davecgh/go-spew` v1.1.1 (ISC) and `github.com/pmezard/go-difflib` v1.0.0 (BSD-3-Clause). The last three are genuinely linked: `otter/v2` v2.3.0 ships a file named `issue_test_1.25.go`, and because that name does not end in `_test.go` Go compiles it as ordinary package code, so its `testify/require` import pulls all three into the binary. Every component remains permissively licensed, so this was an accuracy defect rather than a licensing one, and consumers of **0.6.0 and earlier should treat their copy of the inventory as inaccurate** — the otter v1 to v2 swap landed in 0.6.0 without a regeneration. `NOTICE` separately failed to name `github.com/oasdiff/yaml3`, a fork of `gopkg.in/yaml.v3` carrying the same Canonical attribution, and paraphrased rather than reproduced the CoreOS attribution that Apache-2.0 section 4(d) requires be carried verbatim. Independently of accuracy, neither `NOTICE` nor `THIRD_PARTY_LICENSES.md` was included in the release tarball (which shipped only `LICENSE`) or in the container image (which shipped neither), so the section 4(d) obligation was unmet for every published artifact despite the files existing in the repository; the tarball now carries all three and the image carries them under `/licenses/`. Drift is now gated rather than trusted to a "regenerate before each release" sentence: `.github/scripts/licenses-check.sh` fails when the inventory stops matching `go list -deps ./cmd/server` or when a dependency's NOTICE goes unpropagated, and it runs both on every pull request and at tag time as a prerequisite of the build jobs. Tracked under SOL-152414.

- A `tool_authorization.access_level_groups` grant of a write/action tool (for example `delete-queue-messages`, `disconnect-client`, `clear-queue-stats`, `clear-client-stats`, or any Config-API management tool) while `enable_write_tools: false` is now reported at startup with a `WARN` naming the inert tool and the groups that reference it. Previously such a grant was accepted in silence even though it could never take effect — the tool is skipped at registration and never appears in `tools/list` — so an operator had no signal distinguishing "the policy is live" from "the policy is inert." The startup validator built its known-tool set from the tool manager, which holds every tool unconditionally, while the write gate is applied later at MCP-server registration; the validator now applies the same gate predicate and reports the difference. Staging an RBAC policy ahead of enabling write tools stays supported, so this is a WARN and not a startup failure: one line per inert tool, tools alphabetized and referencing groups deduped within each line, matching the existing `list-brokers` inert-grant WARN. Setting `enable_write_tools: true` activates the grants and silences the WARN. Grants naming a tool the server does not know at all remain a fatal startup error, and `list-brokers` grants keep their existing WARN. Tracked under SOL-152508.

- A required string parameter on a composite tool (`msgVpnName`, `queueName`, `clientName`, `topicEndpointName`, `restDeliveryPointName`, and the like) now carries `minLength: 1` in its generated input schema, so an empty string is rejected at MCP schema validation — before the request is built — with an error naming the offending parameter. Previously an empty string passed validation and reached the broker, which rejected it one round trip later with a generic 4xx that named nothing: when the empty value was a SEMPv2 path segment it produced a double slash (`.../msgVpns//queues/foo/deleteMsgs`), and when it was a create-time object name carried in the request body (`create-queue`'s `queueName`, `create-topic-endpoint`'s `topicEndpointName`, `create-rdp`'s `restDeliveryPointName`, `create-message-vpn`'s `msgVpnName`) it was sent as an empty attribute the broker refused. The native write-action handlers set `minLength: 1` on their path params until SOL-151646 migrated them to composite YAML, where the parameter definition had no way to express it; this restores and broadens the guardrail so it applies uniformly to every composite write tool (`create-queue`, `update-queue`, `delete-queue`, `create-message-vpn`, `update-message-vpn`, `delete-message-vpn`, `create-topic-endpoint`, `update-topic-endpoint`, `delete-topic-endpoint`, `create-rdp`, `update-rdp`, `delete-rdp`, `delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats`), the create-time body names included, and to required string identifiers on read tools as well. The rule keys off the `required`/`string` flags already on each YAML parameter definition — every required string in the catalog is an object identifier for which an empty value is never valid — so no tool YAML changed. A caller that was passing an empty string for one of these parameters now gets a schema-validation error rather than a broker-side 4xx; the request failed either way, but the message is now local and names the parameter. Tracked under SOL-151811.

### Security

- Bumped `github.com/getkin/kin-openapi` from `v0.134.0` to `v0.145.0`, closing 2 Dependabot alerts against `openapi3filter`: a critical fail-open authentication bypass in `ValidationHandler.Load()` (GHSA-r277-6w6q-xmqw) and a medium nil-pointer panic validating a schema-less `content` parameter (GHSA-jpcw-4wr7-c3vq). Neither vulnerable path was reachable in this codebase — the only usage is the `openapi2` subpackage (`internal/semp/sempv2/operation.go`), parsing our own embedded, trusted SEMP specs, never `openapi3filter` — but the bump closes the alerts and keeps the dependency current. No code changes required. Tracked under SOL-152553.
- Preserved cross-origin (Origin/Sec-Fetch-Site) protection on the `/mcp` endpoint across the `github.com/modelcontextprotocol/go-sdk` bump from `v1.5.0` to `v1.6.1`. In v1.5.0, `mcp.NewStreamableHTTPHandler(getServer, nil)` applied default cross-origin checks when `opts` was nil; v1.6.0 removed that default — the SDK now applies no cross-origin protection when `opts.CrossOriginProtection == nil` unless `MCPGODEBUG=enableoriginverification=1` is set at the process, and the option field itself is deprecated for removal in v1.8.0. Without a matching change on our side the bump would have silently dropped browser-side cross-origin defense on `/mcp` — a CSRF-shaped regression from a routine minor-version bump — even though the auth middleware around the handler is unchanged. `cmd/server/main.go` now wraps the SDK handler with `net/http.CrossOriginProtection` per the SDK's own deprecation guidance, so the posture holds beyond v1.8.0 without a runtime env dependency. Behavior on the wire is unchanged from v1.5.x: safe methods (GET/HEAD/OPTIONS) pass, and requests without `Sec-Fetch-Site` or `Origin` (non-browser callers) still pass; only browser-initiated cross-origin non-safe requests are rejected, which is what v1.5.x already did.

- `actions/checkout` steps in `release.yml`, `llm-eval.yml`, `build-and-test.yml`, `ci-pr.yaml`, and `dco.yaml` now set `persist-credentials: false`, matching what `build-image.yaml` and `guardian-scan.yaml` already did. Without it, the job's `GITHUB_TOKEN` stayed in the local git config for the rest of the job, readable by any later step, script, or dependency in that job. `dco.yaml`'s gate does a live `git fetch` against `origin` after checkout (this repo is internal, not public, so that needs auth); that one command now authenticates with its own short-lived header (`git -c http.<url>.extraheader`) instead of relying on the persisted credential, so the fix doesn't regress the DCO check. Tracked under SOL-152962.

- Closed three gaps `.github/ADMIN_SETUP.md` had flagged as still open ahead of going public: GitHub Actions can no longer approve pull requests (SOL-152958 — closed the self-merge path a compromised action + the 1-approval/no-dismissal `main` ruleset made possible), the repo's default Actions workflow token permission is now read-only rather than write (SOL-152959 — every workflow already declared its own explicit `permissions:` block, so this has no functional effect), and the `main-protection` ruleset now requires a Code Owners review and resolved conversation threads before merge, not just any one approval (SOL-152961). All three were applied directly via the GitHub API/ruleset endpoints, not through a PR — there was no code to review. Fork pull request workflow approval (SOL-152960) stays open: GitHub exposes no REST endpoint for it, so it needs a manual change by someone with org-admin access. Found in a Claude-assisted security scan by Reuben D'Souza.

## [0.6.0] - 2026-07-28

### Added
- Broker `auth.mode: oauth` (RFC 8693 token exchange, "Hop 2") is now a fully supported broker authentication mode alongside `basic` and `bearer`. A broker configured with `auth.mode: oauth` obtains its broker-bound token by exchanging the calling agent's Hop 1 token via the `broker_oauth:` block's IdP coordinates — see `docs/authentication.md` for the full Hop 1 / Hop 2 model, config shape, and operational behavior (retries, circuit breaker, `Retry-After` handling, token caching). `mcp_client_auth.mode: oauth` is required whenever any broker uses `auth.mode: oauth`, since Hop 2 consumes the agent's Hop 1 token as the RFC 8693 `subject_token`.
- New MCP tools `list-kafka-receivers`, `get-kafka-receiver-status`, `list-kafka-senders`, and `get-kafka-sender-status` for Kafka Receiver/Sender monitoring — the same list-then-drill-down pattern as `list-rdps`/`get-rdp-status` and `list-bridges`/`get-bridge-status`. A Kafka Receiver pulls messages from an external Kafka cluster into a Message VPN; a Kafka Sender pushes messages from a VPN's queues out to an external Kafka cluster. Unlike bridges' two-directional connection-state model, both objects use a single `up`/`enabled`/`failureReason` status shape (the same shape as RDPs) and a single-name identifier (`kafkaReceiverName`/`kafkaSenderName`, not a compound key). `list-kafka-receivers`/`list-kafka-senders` return up to 100 by default (max 500 via `maxResults`), with a summary block (`downCount`, `disabledCount`, `byFailureReason`); `get-kafka-receiver-status`/`get-kafka-sender-status` return additional per-object detail including topic-binding status (`topicBindingUpCount`/`topicBindingCount`) or queue-binding status (`queueBindingUpCount`/`queueBindingCount`). Read-only monitoring only; write tools and live-broker E2E fixture coverage are deferred to follow-up tickets. Unlike bridges, this implementation is spec-derived rather than lab-verified: neither lab appliance available at implementation time (one on an unreleased build, one on `sempVersion 10.26.0.8586`) exposes the `kafkaReceivers`/`kafkaSenders` SEMP paths at all, so whether `failureReason` reliably populates on a real down receiver/sender — versus staying empty the way bridges' `inboundFailureReason` did — remains unconfirmed; a follow-up should verify against a broker with the feature licensed/enabled. Tracked under SOL-152328.
- New MCP tools `list-bridges` and `get-bridge-status` for Bridge monitoring — the same list-then-drill-down pattern as `list-rdps`/`get-rdp-status`. `list-bridges` returns enabled state, inbound/outbound connection state, and last inbound failure reason for every bridge in a VPN (default 100, max 500 via `maxResults`), with a summary block (`downCount`, `disabledCount`, `byInboundFailureReason`). `get-bridge-status` returns additional per-bridge diagnostic detail — connection establisher, failure category, and client name — for one bridge, identified by `msgVpnName` + `bridgeName` + `bridgeVirtualRouter` (bridges are the only object in this server keyed by two names rather than one, since a bridge's config can differ between a broker's primary and backup virtual router in an HA pair). Tracked under SOL-152124.
- Process-level circuit breaker in front of RFC 8693 token exchange to the IdP, so a sustained IdP outage fails fast instead of driving every broker's requests into the full retry/timeout budget. One breaker per process guards the single shared IdP; because the deployment has one IdP, one client credential, and one shared availability dependency, all brokers and tenants share it. A "request" for the breaker is one complete logical exchange *after* the retry loop has finished — a chain that retries a 5xx three times before giving up records one failure, not three. Failure classification is owned by the server, not the operator: transient network errors, upstream 5xx, and response-body-read failures count toward the breaker; HTTP 429 is excluded (the IdP is reachable and throttling, so counting it would let one tenant's rate-limit trip the shared breaker); token rejections, caller cancellations, and local request-build errors are excluded (they say nothing about IdP availability). Endpoint *misconfiguration* — an untrusted/expired TLS certificate, a hostname mismatch, or a DNS name-not-found for the configured IdP — is also excluded: it is a permanent operator fault, not an outage, and counting it would let one bad config value trip the shared breaker for every tenant with no chance to heal (a DNS *timeout*, by contrast, is transient and does count). Once open, exchanges are rejected immediately with a new `ErrExchangeCircuitOpen` sentinel — classified transient like a transport failure but distinct in the audit line ("we did not try" vs. retries-exhausted's "we tried and gave up"); state transitions log at WARN. Configured under a new optional `broker_oauth.circuit_breaker:` block (nested there because the breaker only protects OAuth token exchange, which exists only when `broker_oauth` does): `enabled` (default true; `false` is an operational escape hatch that disables the breaker while leaving retries intact and logs a WARN), `failure_rate_window`, `minimum_requests`, `failure_rate_threshold_percent`, `consecutive_failure_threshold` (fast-trips a fast-failing outage without waiting for the rate rule's sample; the count is window-bound, so failures spaced out by long retry chains may not accumulate; `0` disables it), `open_state_duration`, and `half_open_probe_requests`. Every field is optional and falls back to a production-safe default, so omitting the block (or the whole thing) yields the shipped defaults with the breaker on; out-of-bounds values are rejected at config load alongside every other `broker_oauth` error. The internal bucket granularity and breaker name are derived, not operator-configurable. Tracked under SOL-151600.
- Shared, process-wide backoff that honors an IdP's `Retry-After` header on an exhausted RFC 8693 token-exchange 429 chain, closing a gap in the circuit breaker's 429-exclusion policy (SOL-151600): because a 429 is deliberately excluded from breaker failure counting, and gobreaker refunds a half-open probe slot for excluded outcomes, a still-throttling IdP could previously admit unbounded sequential probes with no pacing between them. `Retry-After` is parsed per RFC 9110 §10.2.3 (both delta-seconds and HTTP-date forms; an HTTP-date at or before the current instant, or a negative delta-seconds value, floors to no gate rather than a negative one, so IdP clock skew cannot produce a nonsensical result) only when a 429 retry chain fully exhausts — not on an intermediate 429 a later retry in the same chain resolves — and only holds off *subsequent* exchange calls; it does not change the 429-excluded classification itself, nor retry pacing within a single exchange's own chain (which already honors `Retry-After` uncapped per SOL-151520 and is unaffected). The gate is shared across every broker and tenant, deliberately: it sits in front of the same one-shared-IdP circuit breaker (SOL-151600), so a 429 from one broker's exchange paces back every other caller against that IdP, not just the one that triggered it. A gated call fails fast with a new `ErrExchangeRateLimited` sentinel — distinct from `ErrExchangeCircuitOpen` so an operator (or alerting keyed on either) can tell "the IdP is unhealthy" apart from "the IdP is healthy but asked us to wait" — before any breaker bookkeeping or IdP round-trip occurs. The honored duration is capped (default 60s, double the breaker's default 30s open-state window) via a new optional `broker_oauth.retry_after:` block with a single `max_honored_duration` field; omitting the block or the field keeps the shipped default, and an operator-set value replaces it entirely in both directions (a smaller cap can clamp tighter than the default, a larger one can honor longer). Three distinct WARN log lines cover the operator-visible outcomes on every exhausted 429 chain: the gate was set (with the honored duration and expiry), the gate was set but the IdP's requested duration was clamped to the configured cap, or the gate could not be set at all because the IdP sent no `Retry-After` (or sent one that didn't parse) — the last of these is called out explicitly because it means this protection cannot engage for that IdP, distinct from a routine "everything working as intended" log line. Tracked under SOL-152285.
- Server-side retry loop for RFC 8693 token exchange to the IdP. Previously a single 5xx, HTTP 429, or connection error to the IdP failed the tool call outright; the loop now transparently retries up to three attempts total against transient signals (5xx, 429, and connection-level errors — DNS, TLS handshake, body-read partials), honoring the IdP's `Retry-After` header uncapped between attempts via jittered linear backoff. 4xx client errors (400, 401, 403) are deliberately not retried — they will not fix themselves on repeat. The whole retry loop is fenced by a chain-deadline (`context.WithTimeout` derived from the retry knobs — currently 19s worst case with the shipped defaults of 5s per attempt, 2 retries, and 1–2s backoff), which also bounds a hostile `Retry-After` value. When all attempts fail, a new `ErrExchangeRetriesExhausted` sentinel distinguishes "we tried and gave up" from a single-shot transport failure; the last attempt's HTTP status and IdP endpoint survive on the same `*ExchangeError` envelope for the audit line. JWKS refresh and OIDC discovery paths deliberately keep the non-retrying HTTP client — they are read-only lookups where a single failure is the correct signal. Tracked under SOL-151520.
- New top-level `tool_authorization:` configuration block for per-tool authorization based on the caller's group or role memberships as carried by a configurable OIDC claim. Required under `mcp_client_auth.mode: oauth` and refused under any other mode — the `enabled` field must be set explicitly to `true` or `false`, forcing every oauth deployment to make a deliberate choice on tool-level access control. When `enabled: true`, the block also carries `groups_claim_name` (name of the OIDC claim carrying the memberships; defaults to `"groups"`) and `access_level_groups` (a map from group name to the list of MCP tool names that group grants; union semantics — a caller is allowed to invoke a tool when at least one of their groups grants it). `list-brokers` is structurally exempt — every authenticated caller can invoke it regardless of the policy, so a caller can always discover configured broker aliases before invoking any other tool; listing it under a group in `access_level_groups` is inert. Authorization decisions live in a new `internal/authz` package; its `Decision` type implements `slog.LogValuer` to emit only an `allowed` flag and a matched-group count, never the group names themselves, so the sanctioned logging path cannot leak group membership. Tracked under SOL-151875, SOL-151907, and SOL-152293.
- Apache 2.0 LICENSE file for open source compliance
- CONTRIBUTING.md with comprehensive contribution guidelines including DCO requirements
- CODE_OF_CONDUCT.md with Contributor Covenant 2.1
- SECURITY.md with vulnerability disclosure policy and security best practices
- GitHub issue templates for bug reports and feature requests
- GitHub pull request template with comprehensive checklist
- Copyright headers to all Go source files
- Status badges in README.md (build, license, Go version, code of conduct)
- Contributing and License sections in README.md

### Changed

- **BREAKING**: Under `mcp_client_auth.mode: oauth` (production) the server now refuses to start with a plaintext listener — i.e. when neither `tls_cert_file`/`tls_key_file` nor a new top-level `tls_terminated_upstream: true` opt-in is set. Previously such a config started silently and transmitted client bearer tokens and tool results in cleartext while validating as production. TLS termination at an upstream proxy/ingress is a supported pattern, so the fix is enforcement with an explicit opt-out: set `tls_terminated_upstream: true` to acknowledge that TLS is terminated upstream — the server then serves plaintext and logs a startup WARN naming the acknowledgment. Providing `tls_cert_file`/`tls_key_file` terminates TLS at the server as before. Mirrors the existing `allow_remote_unauthenticated` / `allow_insecure_broker_tls` guards; the field is ignored in the `disabled`/`static` dev modes. Migration: an `oauth` deployment without server-side TLS certs must set `tls_terminated_upstream: true` (or configure certs). Because the config parser rejects unknown fields, an older binary will fail to start if the field is present — so deploy the new binary and the updated config together (the field and the binary that understands it move as one change), and when rolling back to an older binary, remove the field in the same change. Completes the server-listener side of the production-mode transport posture begun in SOL-149665. Tracked under SOL-150692.

- **BREAKING**: MCP tool `get-vpn-health` renamed to `get-vpn-status`. Matches the `get-broker-health` → `get-broker-status` precedent (SOL-150707): the tool reports raw VPN state (enabled, connection count, per-service up/down), not a health verdict — whether a VPN is "healthy" depends on deployment intent the server doesn't have. Input parameters and output shape are unchanged; only the tool name and step key (`vpnHealth` → `vpnStatus`) changed. Migration: any client invoking the tool by name must switch to `get-vpn-status`. Tracked under SOL-151742.

- **BREAKING**: The four write-action tools — `disconnect-client`, `clear-client-stats`, `delete-queue-messages`, and `clear-queue-stats` — were migrated from bespoke native Go handlers to composite SEMPv2 YAML definitions, aligning them with every other write tool served by the composite framework. Tool names and input parameters are unchanged, but the **result shape changes**: each tool previously returned a strict, hand-built object (`{ "status": "ok", "msgVpnName": …, "clientName": … }` for the client tools, `{ "status": "ok", "msgVpnName": …, "queueName": … }` for the queue tools, `additionalProperties: false`), and now returns the raw SEMPv2 action envelope keyed by the operation — `{ "disconnect": { "data": {}, "meta": { "responseCode": 200, … } } }`, and likewise `{ "clearStats": … }` for the two stats-clear tools and `{ "deleteMsgs": … }` for `delete-queue-messages`. A caller that asserted on the old `status: "ok"` field or its flat `msgVpnName`/`clientName`/`queueName` echo must read success from the envelope's `meta.responseCode` instead. Migration: update any client that parsed these tools' output to read the SEMPv2 `meta`/`data` envelope rather than the former `{ status: "ok", … }` object; requests are unchanged. All four remain gated behind `enable_write_tools: true`. Tracked under SOL-151646.

- SEMP permission-denied (code 72) errors are now tagged with the calling tool and operation, so an operator can see which tool call hit a broker authorization limit. Tracked under SOL-151718.

- Disambiguated the `clear-queue-stats` and `delete-queue-messages` tool descriptions so agents stop conflating "reset counters" with "purge messages." Tracked under SOL-151850.

- Added a one-line pointer from each of `list-vpns`, `list-queues`, and `list-clients` to its list-then-drill-down "show details" counterpart (`get-vpn-status`, `get-queue-metrics`, `get-client-details` respectively), so an agent that only reads the list tool's description still discovers the detail tool exists — matching the pointer `list-rdps`/`list-bridges` already had to `get-rdp-status`/`get-bridge-status`. No behavior, parameter, or output-shape change; description text only. Tracked under SOL-152122.

- When a write tool is given a body attribute the embedded SEMP schema doesn't recognize, the client-side rejection now names all three possible causes: typo, tool-only param that should be declared as path/query/header, or a broker newer than the embedded schema. The embedded schema version is logged via `slog.Debug` for operator correlation. Previously the message framed the only cause as a naming mistake, hiding the legitimate case where the target broker is newer than the spec the server was built against. Tracked under SOL-152207.

- Removed the dangling "See the SEMP `<Type>` schema for the full list." pointer from the `create-message-vpn`, `update-message-vpn`, `create-queue`, `update-queue`, `create-topic-endpoint`, `update-topic-endpoint`, `create-rdp`, and `update-rdp` write-tool descriptions. The MCP server does not expose the SEMP schema, so the sentence sent agents chasing a resource they cannot reach; the attribute examples already inline in each description remain. Tracked under SOL-152203.

- Reshaped the agent-facing surface for token-exchange failures. The `error` string a tool call returns for a token-exchange failure now collapses from six sentinel-specific messages into two categories: transient (`ErrExchangeTransport` and `ErrExchangeRetriesExhausted`) surface as `"Authentication is unavailable — the identity provider is not responding."` (deliberately not broker-named because a shared-IdP outage affects every broker at once); everything else surfaces as `"Authentication failed for broker \"X\". This is a server-side issue."` with the broker alias interpolated so a multi-broker operator can grep the audit line for context. No IdP-generated content is embedded in these strings, so an IdP that leaks sensitive material in an `error_description` field cannot bleed it into the agent surface. Related: the `retryable` flag on the tool result — which agents may branch on — now returns `false` for `ErrExchangeRetriesExhausted` (we already gave up, retrying doesn't help), for a 4xx response whose body is not a standard OAuth `error` JSON (previously `true` — a proxy/WAF interception page will not become a valid OAuth response on retry), and for an oversized OAuth error code (previously `true`). Sentinel-specific detail (endpoint, HTTP status, elapsed) still lands on the audit log line via `LogAttrs`, which is the right audience for it. Tracked under SOL-151520.

- Updated the embedded SEMPv2 OpenAPI specs to the 10.26.2 rolling release (`10.26.2.9715`), so tool schemas track current broker attributes. The three spec files are renamed to `semp-v2-swagger-{action,config,monitor}.10.26.2.json`. Loading is unaffected — specs are picked up by the embed glob and typed from their `basePath`, not their filename. Tracked under SOL-152206.

- SEMP `429 Too Many Requests` and `503 Service Unavailable` responses are now retried at most 3 times, independently of the configured `retries` cap (default 10). A 429/503 signals an overloaded or unavailable broker, and these retries bypass the per-broker rate limiter, so retrying up to 10 times amplified load precisely when the broker could least handle it. The cap bounds a transient episode to 4 requests (original + 3 retries); once reached, the call fails with the same retries-exhausted error it returned before, only sooner. Other retry behavior is unchanged: the single retry for other 5xx, 401 re-authentication, connection-error retries, and the overall retry-chain deadline all still honor the full `retries` budget. Distinct from the retry-chain deadline (SOL-151518), which bounds duration rather than the number of attempts. Tracked under SOL-152209.

- Swapped the OAuth `TokenCache` backend from `github.com/maypok86/otter` v1.2.4 to `github.com/maypok86/otter/v2` v2.3.0. Internal Go API only; on-the-wire MCP schemas, operator config, and the `TokenCache` interface method set are unchanged. Per-entry TTL is now supplied via an `ExpiryCalculator` configured at cache construction, replacing v1's per-call TTL argument; the wrapper still short-circuits with `PutDroppedTTL` when the caller-supplied `ExpiresAt` yields a non-positive derived TTL. Two `internal/oauth/cache` enum values were removed as unreachable dead code: `GetMissExpired` (both v1 and v2 surface expired entries as absent from `GetIfPresent` internally, so no consumer ever saw this value) and `PutDroppedFull` (v1's admission only rejects when `cost > MaxSize/10` with the default cost function, requiring `MaxSize < 10` which no realistic config sets, and v2's `Set` no longer signals capacity rejection via its return). `GetMissAbsent` was renamed to `GetMiss` — after collapsing the redundant statuses, `GetMiss` is the single non-hit status the wrapper produces, and the previous name incorrectly implied a distinction the wrapper no longer draws. The wrapper's wall-clock double-check in `Get` was also removed after tracing that it cannot fire in v2: v2's `GetIfPresent` runs a nanosecond-precise `HasExpired` internally, and the `ExpiryCalculator` installs each entry with a deadline at or before `ExpiresAt`, so there is no window where the backend returns a fresh entry that the wall-clock check would reject. Callers observe identical behaviour before and after the swap; the six-commit sequence on the branch is designed so each commit compiles and its tests pass. Tracked under SOL-152397.

### Removed

- Per-broker `brokers.<alias>.auth.scopes` field, removed before its first release with the field populated. Rationale: scope entitlement is a property of the user (carried on the subject token), not of the broker — a single static per-broker list either hard-fails less-privileged users (strict IdPs reject `invalid_scope`) or under-scopes admins. The exchange request now omits the RFC 6749 §3.3 `scope` parameter, so the IdP grants its per-client / per-user default scopes. Config decoding is strict (`KnownFields(true)`), so any config still declaring `auth.scopes` fails loudly at startup — this is intentional and covered by a test. Follow-up SOL-151816 will design a per-user scope mechanism; if per-call scopes ever vary for the same subject token, they must also join the token-exchange dedup key (`internal/tokenexchange/dedup_key.go`).

- `broker_oauth.audience_parameter_name` no longer accepts `scope` or `resource`. Both were schema-accepted in 0.5.0 but never implemented at the wire-construction layer, so a config that passed load would have failed later — at Hop-2 runtime construction if a broker actually used `auth.mode: oauth`, or, since `broker_oauth:` is validated whenever the block is present regardless of whether any broker uses oauth mode, even a `broker_oauth:` block staged in advance with one of these values now fails at config load rather than loading cleanly. The allowlist is now `audience` only, and the rejection joins the other `broker_oauth` errors at config load. Migration: a staged `broker_oauth:` block using `scope` or `resource` must switch to `audience`; broker OAuth is not yet usable against an IdP that requires either style.

### Fixed

- The SEMP transport now bounds the TCP connection attempt, so a broker whose network path silently drops packets fails in seconds rather than minutes. Every other connection phase was already bounded — TLS handshake, response headers, expect-continue — but the dial was not, and because the transport is built as a literal rather than cloned from Go's default it fell back to a dialer with no timeout. A dropped SYN, from a security group that omits the server's address range, a default-deny network policy, or a stale DNS answer after a failover, left each attempt waiting the full `semp.request_timeout_duration` while holding one of that broker's concurrency slots; since connection errors are retried, the chain could occupy a slot for roughly 15 minutes at the shipped defaults instead of about 5.5. Only calls to the affected broker are delayed — each broker has its own transport and slots, so one unreachable broker cannot stall a healthy one. The bound is derived from `request_timeout_duration` (half, capped at 10s) rather than hardcoded, so it still fires for operators who tune that value below 10s. Connections that are refused outright were already fast and are unchanged. Tracked under SOL-152403.

- OAuth broker requests now recover from an expired token in-flight. When a broker returns `401 Unauthorized` under `auth.mode: oauth`, the SEMP transport evicts the cached broker token and retries the request once with a freshly exchanged token, instead of failing the call immediately. The recovery decision belongs to the authenticator: `HandleAuthFailure` returns whether to retry and whether the retry must re-authenticate, so static modes (basic, bearer) are unaffected. The re-auth is capped at one attempt per request, so a persistently rejected credential surfaces as a `401` rather than looping. If the token exchange itself fails during the retry (e.g. the IdP is unreachable), the failure is now logged with broker and operation and carries the last observed `401` status. Tracked under SOL-151624.

- `list-vpns` aggregation no longer reports a dead `zeroConnectionCount`; the count now reflects actual per-VPN connection state. Tracked under SOL-151771.

- `delete-queue-messages` and `disconnect-client` are no longer retried when a request fails in a way that leaves it unclear whether the broker already carried it out. Previously a broken connection mid-call could replay a queue purge up to 11 times, destroying messages spooled after the original request while reporting the call as failed; the retry policy inferred replay safety from the HTTP method, and SEMPv2 routes these non-idempotent actions over PUT, which RFC 9110 calls idempotent. The tools' existing `idempotent: false` annotation now drives the retry policy, and the tool result no longer tells the agent to try again — it asks it to check current state first. This holds on a transient broker status as well as a broken connection: a `429`, `503`, or other `5xx` on these two tools now surfaces as a retries-exhausted error flagged non-idempotent, carrying the "check current state first" guidance rather than the raw status. On `429` and `503` this also corrects the agent-facing `retryable` flag, which those two statuses previously set to `true` regardless of the annotation. The broker's own reason is preserved through the change, so a `503` that names a pre-execution rejection (e.g. `Replication Is Standby`) still reaches the caller and still distinguishes "may have been applied" from "definitely was not". Both tools require `enable_write_tools: true`. Every other tool is unaffected, including the `config/` PUT/DELETE write tools and the `idempotent: true` actions `clear-queue-stats` and `clear-client-stats`. Tracked under SOL-152400.

- `semp.request_min_interval` now throttles a broker as a whole, as documented, instead of allowing twice the configured rate. Each protocol client built its own rate limiter, so a broker with both a SEMPv1 and a SEMPv2 client had two — and because a ticker buffers one tick, an idle protocol always had one waiting and its next request was admitted with no spacing at all. An operator setting `100ms` to protect a stressed broker was getting 20 requests/second rather than 10. Concurrency was not required to hit it: any alternation between a SEMPv1 tool (`get-broker-status`, `get-redundancy-status`, `get-discard-stats`) and a SEMPv2 tool was enough, so the exposure covered the whole tool surface. The limiter is now created once per broker and shared by both protocol clients, matching how the in-flight cap `semp.max_concurrent_per_broker` was fixed in SOL-150116, and it is stopped in one place when the broker client closes. The default is `100ms`, so this applies to every deployment that has not disabled throttling. Tracked under SOL-152401.

### Security

- `Token` and `Exchange` types now implement `LogValue`/`String`/`GoString` guards so raw OAuth tokens cannot leak through `slog`, `fmt`, or `%#v` rendering. Tracked under SOL-151649.
- SEMPv2 path-parameter values are now rejected before URL construction when they are empty or a dot segment (`.` or `..`). `buildURL` escaped `/` but left dot segments intact, so a value like `..` could, on a broker or fronting proxy that normalizes dot segments, collapse a request onto an unintended (e.g. parent) path — most consequentially on the destructive `config/deleteMsgVpn`, `config/deleteMsgVpnQueue`, and `action/doMsgVpnQueueDeleteMsgs` operations. The guard rejects the value with a clear error naming the parameter and covers read and destructive operations at the single substitution choke point. Tracked under SOL-152208.

## [0.5.0] - 2026-07-09

### Added
- Action tools for queues and clients (queue actions and client actions). Tracked under SOL-148462.
- Core management tools for VPN and queue administration. Tracked under SOL-148460.
- RDP (REST Delivery Point) write tools. Tracked under SOL-148461.
- `/livez` liveness endpoint, with `/health` retained as an alias. Tracked under SOL-151283.
- `/readyz` readiness endpoint reflecting MCP-server readiness, decoupled from broker reachability. Tracked under SOL-151284.
- Kubernetes split liveness/readiness/startup probes; `/ready` aliased to `/readyz`. Tracked under SOL-151285.
- Correlation-ID HTTP middleware: the ID is forwarded to SEMPv1/v2 broker requests, returned in the MCP response header and `CallToolResult.Meta`, and emitted as a `slog` attribute on every request-scoped log line. Tracked under SOL-151279, SOL-151280, SOL-151281, SOL-151282.
- Whole-mux HTTP panic-recovery middleware. Tracked under SOL-151286.
- SIGTERM in-process drain (`/readyz` flips to 503 → drain → graceful shutdown); a second signal forces immediate shutdown. Tracked under SOL-151288 and SOL-151437.
- `listen_address` server config with a loopback-only default, so the server does not bind a public interface unless explicitly configured. Tracked under SOL-150690.
- RFC 8693 OAuth token-exchange runtime (broker-OAuth "Hop 2"), gated behind `ENABLE_UNRELEASED_BROKER_OAUTH`. Tracked under SOL-150799, SOL-150800, SOL-150801.
- Aggregations for `list-vpns`, `list-rdps`, `list-slow-subscribers`, `list-queue-discards`, and `get-rdp-status`. Tracked under SOL-151313, SOL-151314, SOL-151315, SOL-151316, SOL-151343.
- Accurate live queue-depth reporting. Tracked under SOL-150260.
- New top-level `broker_oauth:` configuration block for upcoming OAuth authentication from the MCP server to brokers. Schema-only in this release — the OAuth runtime is not yet wired, and any broker with `auth.mode: oauth` is rejected at startup with a standalone error banner explaining the limitation and a per-broker validation error in the joined config error. The block holds the global IdP coordinates the MCP server will use to obtain broker-bound tokens: `idp_token_endpoint`, `mcp_server_client_id`, an `mcp_server_client_auth` discriminated union (one named sub-block per IANA "OAuth Token Endpoint Authentication Methods" identifier — V1 supports `client_secret_basic` and `client_secret_post`), `grant_type` (allowlisted to RFC 8693's token-exchange URN), and `audience_parameter_name` (allowlisted to `audience` | `scope` | `resource`). The discriminated-union shape structurally prevents misconfigured method/credential pairings — operators choose a method by populating its named sub-block; the validator enforces "exactly one sub-block populated." Per-broker `auth.mode: oauth` accepts an optional `audience` field whose value is forwarded to the IdP when set. The validator also enforces a permanent structural invariant: if any broker uses `auth.mode: oauth`, `mcp_client_auth.mode` must also be `oauth` (the MCP server cannot obtain a broker token without the agent's token from the client-auth side). Operator-facing banners and errors live in a new `internal/banner` package so future banners land in one canonical home. Tracked under SOL-150796.

### Changed

- **BREAKING**: Top-level client-authentication block renamed from `client_auth:` to `mcp_client_auth:`. The Go type renames to `MCPClientAuthConfig`, and the field on `ServerConfig` renames to `MCPClientAuth`. The rename disambiguates the operator-facing schema now that the new `broker_oauth:` block introduces a separate `broker_oauth.mcp_server_client_auth:` nested sub-block — the top-level `client_auth:` name was ambiguous between client authentication on the inbound side (agents authenticating to the MCP server) and on the outbound side (the MCP server authenticating to the IdP). Migration: rename the top-level `client_auth:` key in every config to `mcp_client_auth:`. No field semantics change; only the block name. Tracked under SOL-150796.

- **BREAKING**: A broker configured with `insecure_skip_verify: true` is now refused at startup under `mcp_client_auth.mode: oauth` (production), unless a new top-level `allow_insecure_broker_tls: true` opt-in is also set. Previously this combination started successfully with only a `slog.Warn`, leaving TLS certificate verification disabled while the server still sent the broker admin credential over the connection on every SEMP request — an exploitable man-in-the-middle path. Mirrors the existing `allow_remote_unauthenticated` guard. `allow_insecure_broker_tls` is server-wide, not per-broker: it lifts the check for every broker in the config, not just the one being onboarded. Dev/static modes are unchanged and continue to allow self-signed brokers without the opt-in. Migration: add `allow_insecure_broker_tls: true` to accept the risk, or install a trusted certificate on the broker. Tracked under SOL-151517.

- Validator now trims whitespace before checking emptiness for basic-auth `username`/`password` and bearer-auth `token`. Configs whose credentials resolve to a whitespace-only string (e.g., `token: " "` or a `${VAR}` substitution that yields only whitespace) are now rejected at startup with a clear "required for X auth" error rather than passing validation and failing every SEMP request with a 401 at runtime. Tracked under SOL-150796.

- Lifted `SafeCookieJar` to the broker level and delegated 401 recovery to the `Authenticator`. Tracked under SOL-151468.
- Unified `downCount` semantics across the `list-*` aggregating tools. Tracked under SOL-151552.

### Fixed
- Bound broker-controlled error text at capture, so an oversized broker error can't blow up memory or logs. Tracked under SOL-151516.
- Recover panics in `errgroup` worker goroutines instead of crashing the server. Tracked under SOL-151514.
- Bound the overall SEMP retry chain with a deadline. Tracked under SOL-151518.

## [0.4.0] - 2026-06-16

### Added
- Error translation and output sanitization for tool responses. Tracked under SOL-148434.
- `get-hardware-details` step for appliance platforms. Tracked under SOL-150708.

### Changed
- **BREAKING**: MCP tool `get-broker-health` renamed to `get-broker-status`. The tool reports raw broker state, not a health verdict. Migration: any client invoking the tool by name must switch to `get-broker-status`. Tracked under SOL-150707.

- Replaced the package-level `auth.AddAuth(ctx, req, cfg)` dispatcher with an `auth.Authenticator` interface and per-broker instances. `NewBrokerClient` is the single builder: it constructs one `Authenticator` per broker from `brokerCfg.Auth` and passes the same pointer to both the SEMPv1 and SEMPv2 protocol clients. The clients no longer read `brokerCfg.Auth`; they store the Authenticator on the struct and call `c.authenticator.AddAuth(ctx, req)` per request. No behavior change for existing `basic` and `bearer` auth modes — same Authorization headers, same retry/timeout posture, same config schema. Internal Go API: `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient` signatures gained an `auth.Authenticator` parameter; these are only called from `semp.NewBrokerClient` and tests in the same module. Enables the upcoming OAuth Token Exchange (Hop 2) support without further protocol-client branching. Tracked under SOL-150794 and SOL-150795.

- Enabled retries for read-only SEMPv1 `show` commands. Tracked under SOL-150664.
- Enforced a per-broker connection bound via `MaxConnsPerHost` and a shared in-flight semaphore. Tracked under SOL-150665 and SOL-150116.

### Fixed
- Truncate broker-controlled error text at capture. Tracked under SOL-150663.
- Cap inbound `/mcp` request body at 4 MiB. Tracked under SOL-150660.
- Close the response body when SEMP retries are exhausted. Tracked under SOL-150661.
- Recover tool-handler panics instead of crashing the server. Tracked under SOL-150685.

### Security
- Keep secrets out of YAML config parse errors in startup logs. Tracked under SOL-150666.

## [0.3.0] - 2026-06-03

### Added
- Per-invocation caller identity on tool-invocation log lines (`sub`, `iss`, `client_id`, `jti`) in `oauth` and `static` client-auth modes. Missing optional claims surface as the `<absent>` sentinel so log consumers see a stable schema; `disabled` mode emits no identity fields. Tracked under SOL-149606.
- Basic health endpoints. Tracked under SOL-148426.
- Advanced monitoring tools: replication, discards, and slow subscribers. Tracked under SOL-148432.

### Changed

- **BREAKING**: Client auth config consolidated into single required `client_auth.mode` enum (`disabled` | `static` | `oauth`). The legacy `development_mode` flag is deprecated and ignored — its presence in YAML logs a deprecation warning at startup. The previous "development_mode + empty dev_token = silent no-auth" path (SOL-149921) is replaced by the explicit `mode: disabled`. Migration:

  | Old config | New config |
  |---|---|
  | `development_mode: true` + `dev_token: "abc"` | `client_auth: { mode: static, dev_token: "abc" }` |
  | `development_mode: true` + missing/empty `dev_token` | `client_auth: { mode: disabled }` |
  | `development_mode: false` + OIDC fields | `client_auth: { mode: oauth, issuer, audience, resource_url }` |

  `mode: oauth` is the only legal production mode and enforces `https://` on broker URLs, issuer, and resource_url. `mode: disabled` and `mode: static` are development-only and allow `http://`. A prominent WARN-level boot banner fires for `disabled` and `static` modes. Tracked under SOL-149989.

- **BREAKING**: Broker aliases must now satisfy a contract: 1–63 characters, only letters/digits/hyphens, must start and end alphanumeric, compared case-insensitively. Configs that previously loaded silently will now be rejected at startup if they contain: empty aliases, whitespace, underscores, dots, embedded special characters, leading or trailing hyphens, aliases longer than 63 characters, or case-only collisions (e.g. `Prod` and `prod` in the same config). Original casing is preserved in all user-facing output (logs, `list-brokers`, error messages); tool calls resolve case-insensitively so any casing of a configured alias works. Migration:

  | Old alias | New alias |
  |---|---|
  | `prod_east` | `prod-east` |
  | `Prod` + `prod` (collision) | rename one of them |

  Tracked under SOL-149789.

- `ToolManager.CallTool` now takes a trailing `Identity` argument carrying per-invocation audit identity. This is an internal Go API (the package is `internal/tools`); on-the-wire MCP tool schemas and operator-visible config are unchanged. Tracked under SOL-149606.

- Config loader now rejects unknown YAML fields at startup instead of silently ignoring them. Previously, a typo like `developmnet_mode` or `insecure_skip_verfy` was accepted and the operator's intended override became a no-op. The loader now fails fast with an error naming the offending field. Existing configs with stale or misspelled keys will fail to start until the typo is corrected; configs with only valid keys are unaffected. Tracked under SOL-149927.

- Skip environment-variable substitution inside YAML comments. Tracked under SOL-149904.

### Deprecated
- The `development_mode` config flag is deprecated in favor of `client_auth.mode` (see the consolidation entry under Changed). It is still parsed but ignored, and logs a deprecation warning at startup. Tracked under SOL-149989.

### Removed
- **BREAKING**: `get-dmr-status` tool removed. Migration: clients invoking `get-dmr-status` must stop; DMR state is available through the remaining monitoring tools. Tracked under SOL-150316.

### Fixed
- OIDC token verifier now bounds the HTTP client used by go-oidc for both startup discovery and lazy JWKS refresh (10s per-request timeout). Previously, the verifier fell back to `http.DefaultClient` (zero timeout), and a slow or hung identity provider during key rotation could wedge the JWKS-refresh goroutine indefinitely and stall per-request token verification past the inbound MCP request's own server-side deadlines. The existing 30s discovery deadline is preserved. Operators running an IdP that legitimately takes longer than 10s to serve `/jwks` will see auth fail closed; document the timeout if your environment requires tuning. Tracked under SOL-150219.
- Wrap the cookie jar in an atomic `SafeCookieJar` to remove a 401 race. Tracked under SOL-149922.
- Cap broker response body at 16 MiB. Tracked under SOL-149920.
- Set granular timeouts on the SEMP HTTP transport, and `ReadTimeout`/`IdleTimeout` on the MCP HTTP server. Tracked under SOL-149925 and SOL-149914.
- Close the broker pool on graceful shutdown. Tracked under SOL-149926.

### Security
- Redact userinfo credentials in broker-URL validation errors. Tracked under SOL-149923.
- Warn on `insecure_skip_verify: true` in production mode. Tracked under SOL-149928.

## [0.2.0] - 2026-05-15

### Added
- SEMPv1 SEMP API client foundation. Tracked under SOL-148424.
- VPN-level monitoring tools: health, list, queues, and clients. Tracked under SOL-148429.
- Remaining monitoring tools: DMR, message rates, and RDP. Tracked under SOL-148433.
- `get-broker-health` tool. Tracked under SOL-148428.
- `select=` field filtering across the monitoring tools to reduce the response size returned to the LLM. Tracked under SOL-149404.
- Pagination page-count cap to prevent infinite loops. Tracked under SOL-149750.
- CODEOWNERS file. Tracked under SOL-149790.

### Changed
- **BREAKING**: Reject `http://` broker and auth URLs in production mode. Migration: use `https://` for broker and auth URLs in production, or run in a development auth mode. Tracked under SOL-149665.
- Rate limiting, retry logic, and MCP error translation. Tracked under SOL-148425.
- Skip retry for non-idempotent HTTP methods (POST/PATCH). Tracked under SOL-149746.
- Reject unknown fields in the composite YAML loader. Tracked under SOL-149670.
- Tune the HTTP transport connection pool (`MaxIdleConnsPerHost`). Tracked under SOL-149748.
- Fix misleading `list-*` tool descriptions that claimed to return all results. Tracked under SOL-149343.

### Fixed
- `.env` parser: strip quoted values and warn on an unreadable file. Tracked under SOL-149664.
- Add `sync.RWMutex` to `ToolManager` to guard concurrent handlers-map access. Tracked under SOL-149671.
- SEMPv2 HTTP headers: omit `Content-Type` on bodyless requests, add `Accept`. Tracked under SOL-149672.
- `buildURL` errors on an unfilled path-parameter placeholder. Tracked under SOL-149749.
- Replace `os.Exit(1)` in the `ListenAndServe` goroutine with an error channel. Tracked under SOL-149747.
- Fix server startup/shutdown logging gaps. Tracked under SOL-149595.

## [0.1.0] - 2026-04-24

### Added
- Initial release of Solace Broker MCP Server
- **Tool Manager Foundation**
  - Generic tool registration and routing infrastructure
  - Parameter and output validation against JSON Schema
  - Broker resolution and connection pooling
  - Structured logging for all tool invocations
- **Composite Tool Engine**
  - YAML-driven multi-step tool definitions
  - Go template-based argument resolution
  - Parallel and sequential step execution
  - Configurable result strategies (collect, merge, unwrap)
- **SEMP Client Layer**
  - HTTP client with Basic Auth and Bearer token support
  - OpenAPI spec parser (799 operations from Monitor, Config, Action APIs)
  - Lazy broker connection pooling with thread-safe double-checked locking
  - Per-broker HTTP transport and connection pooling
- **Configuration Management**
  - YAML config file with environment variable substitution (`${VAR_NAME}`)
  - `.env` file loading for local development
  - Validation for broker URLs, auth modes, ports, TLS pairing
  - Multiple broker support with independent credentials
- **Authentication & Security**
  - OAuth/JWT token validation with OIDC provider integration
  - Development mode with optional static dev token
  - Automatic JWKS key rotation
  - Scope-based access control (optional)
  - OAuth 2.0 Protected Resource Metadata endpoint (RFC 9728)
- **Secure Logging**
  - Structured JSON logging with `log/slog`
  - Credential redaction via `slog.LogValuer` pattern
  - `ReplaceAttr` safety net for defense-in-depth
  - Configurable log levels (debug, info, warn, error)
  - Never logs passwords, tokens, or authorization headers
- **Testing Infrastructure**
  - Unit tests across all packages with `-race` detector
  - Integration tests for tool manager and handlers
  - E2E test suite with two-broker Docker Compose setup
  - OAuth integration tests with Keycloak
  - Comprehensive test coverage (config, semp, composite, tools, auth)
- **CI/CD Pipeline**
  - GitHub Actions workflow for lint, build, test, E2E
  - golangci-lint with security checks (gosec, bodyclose, noctx)
  - E2E tests run automatically on all PRs
  - OAuth E2E tests with Terraform-managed Keycloak
- **Production Deployment**
  - Dockerfile with multi-stage build and non-root user
  - Kubernetes manifests (Deployment, Service, ConfigMap, Secret)
  - GitHub Actions release workflow with multi-platform binaries
  - Health check endpoint (`/health`)
  - Graceful shutdown with 120-second timeout
- **Documentation**
  - Comprehensive README with quickstart guide
  - Architecture documentation with component diagrams and request flow
  - Secure logging rules with examples
  - E2E testing guide
  - Packaging and release documentation

### Changed
- Upgraded MCP Go SDK to v1.5.0

### Security
- All credentials redacted from logs by default
- Constant-time comparison for dev token validation (prevents timing attacks)
- TLS certificate verification enabled by default (`insecure_skip_verify: false`)
- HTTP server ReadHeaderTimeout set to prevent Slowloris attacks

## [0.0.1] - 2026-02-15

### Added
- Initial proof-of-concept implementation
- Basic SEMP client
- Simple config loading

---

## Versioning

This project uses [Semantic Versioning](https://semver.org/):
- **MAJOR** version for incompatible API changes
- **MINOR** version for new functionality in a backward-compatible manner
- **PATCH** version for backward-compatible bug fixes

## Release Process

1. Update this CHANGELOG with all changes in `[Unreleased]`
2. Move unreleased changes to a new version section with date
3. Create git tag: `git tag -a v0.2.0 -m "Release v0.2.0"`
4. Push tag: `git push origin v0.2.0`
5. GitHub Actions automatically builds binaries and creates GitHub Release

## Links

- [Unreleased]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.7.1...HEAD
- [0.7.1]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.7.0...v0.7.1
- [0.7.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.6.0...v0.7.0
- [0.6.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.5.0...v0.6.0
- [0.5.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.4.0...v0.5.0
- [0.4.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.3.0...v0.4.0
- [0.3.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.2.0...v0.3.0
- [0.2.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.1.0...v0.2.0
- [0.1.0]: https://github.com/SolaceProducts/solace-broker-mcp/compare/v0.0.1...v0.1.0
- [0.0.1]: https://github.com/SolaceProducts/solace-broker-mcp/releases/tag/v0.0.1
