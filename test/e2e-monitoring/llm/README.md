# LLM e2e eval suite

LLM-driven e2e test harness for the broker MCP server, using the Claude Code
CLI as the agent. Sends NL prompts, captures `stream-json` output, and asserts
on tool choice, answer fidelity, and refusal behavior. Thirteen scenarios cover
the e2e-monitoring fixtures F1–F7 plus two safety cases — three opt into
running on both `broker-a` and `broker-b`, the rest run on `broker-a` only
(see [Per-scenario broker selection](#per-scenario-broker-selection)).

## Quickstart

```sh
# 1. Brokers, fixtures, broker-drivers, MCP server — all left running.
./setup-fixtures.sh

# 2. Run the suite. Flakes? Re-run the suite — no built-in retry.
./run-all.sh                          # all scenarios
./run-all.sh --fail-fast              # stop after the first failing scenario
./run-all.sh --no-precheck           # skip MCP/broker reachability check

# 3. Or one scenario directly.
./run-scenario.sh scenarios/f5-composition.json

# 4. Tear down when done (brokers stay up).
./teardown-fixtures.sh
```

Exit codes: `0` pass, `1` assertion failed, `2` invocation error.
Prereqs on PATH: `claude`, `jq`, `envsubst`, `docker`, `gcc`.

### Claude CLI version

The pin lives in [`package.json`](package.json) (single source of truth);
`config.env` reads it via `jq`. The runner refuses to start on a
`claude --version` mismatch.

The pin tracks npm's `stable` dist-tag — deliberately lagged from
`latest` so a fresh release can't silently flip scenarios. Renovate
([`.github/renovate.json`](../../.github/renovate.json)) watches this
package only and opens a weekly PR when `stable` moves; never auto-merges.

Install however you like — local dev can use any of:

```sh
claude install $(jq -r '.dependencies."@anthropic-ai/claude-code"' package.json) --force
npm install -g @anthropic-ai/claude-code@$(jq -r '.dependencies."@anthropic-ai/claude-code"' package.json)
npm ci  # uses the lockfile; binary lands at ./node_modules/.bin/claude
```

CI uses the `npm ci` path so the install matches the FOSSA-scanned
lockfile exactly. The runner also exports `DISABLE_AUTOUPDATER=1` so the
background updater can't drift the version mid-suite. Set
`CLAUDE_VERSION_PIN_OVERRIDE=1` to skip the check locally.

**Bump procedure** (manual or reviewing a Renovate PR):

1. Bump the version in `package.json` and regenerate the lockfile
   (`npm install --package-lock-only`). Renovate's PR does both.
2. Run `./run-all.sh` twice against the candidate — confirm every
   scenario passes both times.
3. Merge.

## Configuration

Both runners source [`config.env`](config.env) at startup. It holds the
target-independent settings (LLM endpoint, CLI version pin) and selects
a **broker target** — a file under [`targets/`](targets/) that supplies
`MCP_URL`, `MCP_BEARER_TOKEN`, `PRECHECK_BROKERS`, and `MCP_URL_DOWN`.

Every value uses `${VAR:-default}`, so anything you export beforehand
wins:

```sh
BROKER_TARGET=lab-broker LLM_SERVICE_API_KEY="$VAULT_KEY" ./run-all.sh
```

`LLM_SERVICE_API_KEY` is never defaulted in `config.env` — set it in the
shell (CI: pull from vault). Three auth modes:

| `LLM_SERVICE_ENDPOINT` | `LLM_SERVICE_API_KEY` | Result |
| --- | --- | --- |
| set (proxy URL) | set | Claude Code routed through the proxy via `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` |
| unset | set | Direct `api.anthropic.com` via `ANTHROPIC_API_KEY` |
| unset | unset | CLI's ambient `~/.claude` login (local-dev convenience; not used in CI) |

`LLM_SERVICE_MODEL_NAME`, when set, becomes the `--model` flag.

### Broker targets

`BROKER_TARGET` (default `local-docker`) names a file under `targets/`:

| Target | File | Notes |
| --- | --- | --- |
| `local-docker` | [`targets/local-docker.env`](targets/local-docker.env) | Default — Docker brokers + MCP server spun up by `./setup-fixtures.sh` |
| Custom (e.g. `lab-broker`) | `targets/<name>.env` (gitignored) | Copy [`targets/lab-broker.env.example`](targets/lab-broker.env.example), fill in the lab MCP endpoint + token |

`./setup-fixtures.sh` and `./teardown-fixtures.sh` only provision/clean up
for `local-docker`; other targets are assumed to be running already at
`$MCP_URL`. For non-local targets, `run-all.sh` ignores each scenario's
`brokers` field (those names refer to local fixtures) and runs every
scenario once against the first `PRECHECK_BROKERS` entry. Fixture-dependent
scenarios will fail loudly unless the lab broker happens to have the
equivalent state.

## Layout

```
config.env                 suite-wide config (sourced by both runners)
targets/<name>.env         per-target overrides (MCP_URL, tokens, brokers)
scenarios/<name>.json      one test case = one prompt + assertions
run-scenario.sh            generic single-scenario runner
run-all.sh                 suite wrapper — precheck, run, table
setup-fixtures.sh          bootstraps local brokers + fixtures (local-docker only)
teardown-fixtures.sh       reverse — leaves containers up
mcp-config.json.tmpl       MCP server pointer (envsubst-rendered to MCP_URL)
mcp-config-down.json.tmpl  deliberately broken (MCP_URL_DOWN) for the down test
package.json + .lock       pinned Claude Code CLI (FOSSA-scannable, Renovate-tracked)
```

## Scenarios

Each scenario is a self-contained JSON file naming what to ask and what to
assert. All thirteen are designed around things the direct tool tests **can't**
express — tool choice under ambiguity, multi-tool composition, numeric
fidelity, refusal — not as a port of the direct test catalog.

| ID | Fixture | Brokers | What it proves |
| --- | --- | --- | --- |
| `f1-list-vpns` | F1 | a | Entity-set fidelity — answer's VPN names match `list-vpns` output exactly |
| `f1-vpn-health` | F1 | a | Tool disambiguation (VPN name → `get-vpn-health`) + healthy-state reporting on the `default` VPN |
| `f1-broker-status` | F1 | a | Tool disambiguation (broker-level prompt → `get-broker-status`, not VPN tools) + operational-state reporting on a clean broker |
| `f2-unbound-queues` | F2 | a | Reasoning over tool result — filter `bindCount==0` |
| `f3-subscriptions` | F3 | a, b | Multi-arg parameterization — pulls VPN + client name out of the prompt |
| `f3-client-healthy` | F3 | a, b | False-positive resistance — must NOT invent problems in clean state |
| `f4-message-rate` | F4 | a | Numeric fidelity — answer's rate falls in a sensible range |
| `f5-detect` | F5 | a | Path tolerance — multiple valid tool routes accepted |
| `f5-composition` | F5 | a | Diagnosis grounded in current broker state |
| `f6-slow-subscriber` | F6 | a, b | Exercises `list-slow-subscribers` — only MCP tool for the per-client `slowSubscriber` flag |
| `f7-causal` | F7 | a | Causal explanation cites the real cause (spool/quota) |
| `safety-mcp-down` | — | a | MCP unreachable → zero tool calls, zero fabricated VPN names |
| `safety-nonexistent-broker` | — | a | Refuses or clarifies, doesn't fabricate broker-z state |

Total per `./run-all.sh`: **16 rows** (10 broker-a-only + 3 × 2 brokers).

### Per-scenario broker selection

Most scenarios run on `broker-a` only — duplicating them on `broker-b` adds
no coverage because the fixture state, expected tool calls, and expected
answer shape are identical across brokers. The three F3/F6 scenarios opt
into both brokers because `$F3_CLIENT_NAME` and `$F6_SUB_CLIENT_NAME` differ
per broker (`...-a` vs `...-b`), which proves the agent extracts the name
from the prompt rather than memorizing one.

A scenario declares its broker set via a top-level array:

```json
{ "brokers": ["broker-a", "broker-b"], "prompt": "..." }
```

Default is `["broker-a"]` when the field is missing. To run a single
scenario against a specific broker:

```sh
BROKER=broker-b ./run-scenario.sh scenarios/f3-subscriptions.json
```

`run-all.sh` reads the field; `run-scenario.sh` ignores it and uses
`$BROKER` directly. Scenarios reference `$BROKER` in the prompt and use the
unsuffixed `$F3_CLIENT_NAME` / `$F6_SUB_CLIENT_NAME` aliases that the runner
points at the right per-broker fixture var.

## Scenario schema

```json
{
  "prompt": "What VPNs are configured on $BROKER?",
  "brokers": ["broker-a", "broker-b"],
  "mcp_config": "mcp-config-down.json.tmpl",
  "expected_tool": "mcp__solace-broker__list-vpns",
  "expected_tool_any_of": ["...", "..."],
  "expected_tools_all_of": ["...", "..."],
  "expected_tools_none": true,
  "ground_truth": {
    "jq": ".vpns.data[].msgVpnName",
    "answer_regex": "[a-z][a-z0-9_-]*-vpn|default"
  },
  "required_substrings": ["test-vpn"],
  "required_substrings_any_of": ["healthy", "operational", "up"],
  "forbidden_substrings": ["prod-vpn"],
  "numeric_match": { "regex": "[0-9]+\\s*msg", "min": 50, "max": 10000 }
}
```

All fields are optional except `prompt`. Use the smallest set the scenario
needs. Field semantics:

- **`brokers`** — array of brokers `run-all.sh` iterates this scenario over.
  Default `["broker-a"]`. See [Per-scenario broker selection](#per-scenario-broker-selection).
- **`mcp_config`** — relative path to an MCP config (default `mcp-config.json`).
- **`expected_tool` / `_any_of` / `_all_of` / `_none`** — tool-choice
  assertions. `_none: true` asserts zero calls (for the MCP-down test).
- **`ground_truth.jq`** — applied to the matching `tool_result`'s parsed
  content to produce the *truth set* of entities.
- **`ground_truth.answer_regex`** — applied to the model's final answer to
  produce the *answer set*. Runner asserts both sets are equal (catches both
  omissions and fabrications).
- **`required_substrings` / `forbidden_substrings`** — must / must-not appear
  in the answer (case-insensitive).
- **`required_substrings_any_of`** — at least ONE must appear (case-insensitive).
  Use when paraphrases are equally valid (e.g. healthy / operational / up) and
  pinning a single literal would be brittle.
- **`numeric_match`** — extract first number matching `regex`, assert
  `min ≤ n ≤ max`. Useful when the test cares about a rate or count, not a
  named entity.

### Fixture-name substitution

Scenario JSONs can reference fixture variables (`$F3_CLIENT_NAME_A`,
`$F5_QUEUE`, `$F7_SPOOL_QUEUE`, etc.). The runner sources
`test/e2e-monitoring/helpers.sh` and expands them with `envsubst` before
parsing. Unset references fail loudly with a hint to run `setup-fixtures.sh`
first.

## MCP wiring

`mcp-config.json.tmpl` is envsubst-rendered to a temp file at run time,
pointing Claude Code at `$MCP_URL` with `$MCP_BEARER_TOKEN`. Defaults are
the local-Docker e2e MCP server (`localhost:9090`) and the static dev
token from `config.env`. Three non-obvious knobs the runner always sets:

- **`--strict-mcp-config`** — `--mcp-config` is additive by default; without
  strict mode Claude Code merges in the user's ambient MCP config
  (`atlassian`, `solace-lab-brokers`, project `.mcp.json`, etc) and the model
  can pull data from those into the answer. Confirmed in an early run that
  fabricated lab broker aliases.
- **`--tools ""`** — disables every built-in tool (Bash, Read, WebSearch,
  …). Without it, on the MCP-down test claude falls back to `Bash` and
  `Read` to investigate why MCP didn't load (observed on 2.1.153).
- **`--allowed-tools "mcp__solace-broker__*"`** — auto-approve list. In
  `--print` mode there's no human to approve tool calls, so anything not in
  this list gets *auto-denied* — "I need permission to run X, please
  approve and I'll retry." We use a wildcard so the suite tracks any new
  MCP tool the server adds without scenario-by-scenario edits. The
  assertion logic, not this flag, is what catches a wrong tool choice.

## Pre-check

`run-all.sh` does a one-time reachability check before iterating scenarios:
sends an MCP `initialize` to `$MCP_URL`, then calls `get-broker-status` on
every broker in `$PRECHECK_BROKERS` (defaults to `broker-a broker-b`).
Every listed broker must respond — the F3/F6 scenarios run on broker-b, so
a precheck that only covered broker-a would let a half-up fixture set
masquerade as healthy. If any step fails, the suite exits with a clear
`[PRECHECK FAIL]` and a hint to run `./setup-fixtures.sh`. This prevents
the "N weird fails + 1 passing safety-mcp-down" pattern that happens when
scenarios run against a dead broker — and saves you ~$0.20 of API credits
per accidental run.

`--no-precheck` skips it (useful when running just the safety-mcp-down
scenario in isolation, or against a non-default MCP setup). Single-scenario
runs via `./run-scenario.sh` don't precheck; the wrapper is the suite-level
guardrail.

## Output

Each scenario streams compact log lines live to the terminal, using the same
`[INFO]` / `[PASS]` / `[FAIL]` colour scheme as `test/e2e-monitoring/helpers.sh`.
Sample run of two scenarios:

```
[PRECHECK PASS] http://localhost:9090 reachable, broker-a get-broker-status returned Solace PubSub+ Standard Version 10.25.0.237
[PRECHECK PASS] http://localhost:9090 reachable, broker-b get-broker-status returned Solace PubSub+ Standard Version 10.25.0.237
[INFO]  Claude CLI Version: 2.1.153
[INFO]  Broker target: local-docker (http://localhost:9090)

════════ [1/2] f1-list-vpns [broker-a] ════════
[INFO]  prompt: What VPNs are configured on broker-a?
[INFO]  tool call: list-vpns {"broker":"broker-a"}
[INFO]  answer: Broker-a has 2 VPNs configured:  ($0.0277, claude-opus-4-7)
[INFO]  expected set:    default test-vpn
[INFO]  answer set:      default test-vpn
[PASS]  f1-list-vpns [broker-a]

════════ [2/2] f1-vpn-health [broker-a] ════════
[INFO]  prompt: Is the default VPN healthy on broker-a?
[INFO]  tool call: get-vpn-health {"broker":"broker-a","msgVpnName":"default"}
[INFO]  answer: Mostly healthy — `default` VPN is enabled, …  ($0.0419, claude-opus-4-7)
[PASS]  f1-vpn-health [broker-a]

════════════════════════════════════════════════════════
scenario                                     result
────────────────────────────────────────────────────────
f1-list-vpns [broker-a]                      PASS
f1-vpn-health [broker-a]                     PASS
════════════════════════════════════════════════════════
Summary: PASS=2  FAIL=0  (of 2)  total cost: $0.0696
```

What each line is:

- `[precheck ok]`, `[INFO] claude CLI:`, the header, `[INFO] expected/answer
  set:`, `[PASS]`, `[FAIL]`, the table, `total cost:` — all from the **test
  harness** (`run-all.sh` / `run-scenario.sh`).
- `[INFO] tool call:`, `[INFO] answer:  ($cost, model)` — reformatted from
  the **Claude Code CLI**'s own `stream-json` output (its `tool_use` and
  `result` events). The cost and model name are fields claude itself
  reports in the `result` event; the harness just shapes them into one
  line. Zero settings.json reading, zero MCP-server traffic.

Under the hood the runner tees the full `stream-json` to a `mktemp` file
that jq parses for assertions and pipes through a summarizing jq filter for
what the user sees. An `EXIT` trap deletes the temp file unconditionally —
no persistent transcripts. No retries: re-run the suite if a scenario flakes.

## Design notes

- **MCP-down → refusal works** with `--strict-mcp-config` + `--tools ""`.
  `--allowed-tools` doesn't affect the refusal path (no tools to call when
  MCP is down) but is still needed for the happy path — see MCP wiring.
  The `Bash`/`Read` fallback observed on 2.1.153 is what `--tools ""` blocks.
- **`--mcp-config` is additive** — always pair with `--strict-mcp-config` in
  tests.
- **Pin the CLI to npm's `stable` dist-tag**, not `latest`. `latest` is
  unvetted by Anthropic; a fresh release can change tool-call wrapping or
  refusal phrasing in ways that silently flip scenarios.
