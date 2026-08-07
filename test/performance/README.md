# Performance test suite

Load-test harness for the MCP server. Spins up a fake fleet of Solace brokers
(`mock-semp`), drives concurrent MCP tool calls (`loadgen`), samples MCP and
mock resource use (`memsampler`, `sampler.sh`), and gates every run behind a
byte-for-byte fidelity check against golden captures from the real broker
(`fidelity`).

> **The fixtures are not in this repo — you generate them, always as one pass.**
> Both sets — `mock-semp/canned/*.json` and `*.xml` (the replayed SEMP
> responses) and `fidelity/golden/*.json` (the expected tool output) — are
> captures from a real broker at one instant, so they stay untracked and out of
> the open-source tree. A fresh clone builds fine and every other suite is
> unaffected, but this one refuses to run until you capture:
>
> ```
> CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
> ```
>
> Never hand-edit the captures, never regenerate one set without the other, and
> never combine captures from separate sessions: the exact-mode gate compares
> byte-for-byte, and self-changing fields (uptime, memory %, disk usage) drift
> the moment the two captures diverge in time. `run.sh` and `run-loadgen.sh`
> check all three properties before they start anything — see
> [Fidelity gate](#fidelity-gate).

Point-of-truth for the plan and metric bars: `docs/internal/architecture.md`
and the per-command headers in `mock-semp/main.go`, `loadgen/main.go`,
`fidelity/main.go`, `memsampler/main.go`.

## Layout

```
mock-semp/        replayer: pretends to be N brokers on 18081..18081+N-1
loadgen/          concurrent MCP tool caller, prints throughput/latency/errors
fidelity/         hard gate: compares tool output vs fidelity/golden/*.json
memsampler/       polls /proc/<pid>/status, writes CSV
sampler.sh        CPU + RSS/PSS/USS for MCP + mock + box totals, CSV
loadgen-sampler.sh   loadgen-side connection/goroutine counters
summary.sh        prints a one-page rollup of a run directory

broker-config.mock.yaml   MCP config pointing at mock-semp (50 brokers)
broker-config.real.yaml   MCP config pointing at the real lab broker

build.sh          builds mock-semp, loadgen, fidelity, memsampler into ./bin/
run.sh            single-host smoke run (mock + MCP + loadgen on one box)
run-mcp.sh        split-host — Box B: MCP + samplers
run-loadgen.sh    split-host — Box A: mock + loadgen + samplers
regen-golden.sh   capture both fixture sets from the real broker, in one pass
fixtures-manifest.sh  records/verifies the capture (hashes, time, provenance)

mock-semp/canned/     replayed SEMP responses  ─┐ gitignored: lab captures.
fidelity/golden/      expected tool output     ─┘ regen-golden.sh writes both.
fixtures.manifest     what the last capture produced (gitignored)
```

Artifacts land in `bin/runs/<timestamp>[-<tag>]/`.

## Ports

| port | who | notes |
|---|---|---|
| `9090` | MCP server | health at `/health`; `run.sh` refuses to start if occupied |
| `18081..18081+N-1` | mock-semp broker ports | one per fake broker; default N=50 → `18081..18130`. In split-host mode Box A binds `0.0.0.0` so Box B can reach these over the LAN |
| `19000` | mock-semp config endpoint | `POST /_mock/config` for per-port latency / error injection. Bound to localhost by default (separate from `-listen-addr`) so opening broker ports to the LAN doesn't also expose the injection knob |

## Quick start (single host)

```
./build.sh
CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh   # once, needs a real broker
./run.sh                          # 32 clients, 60s
CLIENTS=200 DURATION=2m ./run.sh
```

The `regen-golden.sh` step is a one-time prerequisite per checkout — the
fixtures it captures are not in the repo. Re-run it whenever you want the mock
replaying current broker data; `run.sh` warns when the capture it's about to
use is more than a week old.

`run.sh` starts `mock-semp` on `:18081..:18130`, runs MCP against
`broker-config.mock.yaml` on `:9090`, runs the fidelity gate, then holds
samplers alongside `loadgen`. If fidelity fails, the load run is aborted.

Key env knobs (full list in `run.sh` header):

| var | default | note |
|---|---|---|
| `CLIENTS` | 32 | MCP sessions in parallel |
| `DURATION` | 60s | Go duration string |
| `TOOLS` | `get-broker-status,list-queues` | subset of tools the mock can answer |
| `LATENCY_MS` | 0 | per-response sleep in mock; use to force per-broker semaphore queueing inside MCP |
| `ERROR_RATE` | 0 | probability each broker response is injected as an error |
| `ERROR_COUNT` | 0 | cap on injected errors per broker port (0 = unlimited) |
| `ERROR_STATUSES` | `503:70,429:20,500:10` | weighted status pool; only 429/500/502/503/504 accepted |
| `BROKER_ALIAS` | `broker-01` | fidelity `-broker`; must exist in `broker-config.mock.yaml` |
| `VPN` | `vpn_1` | fidelity `-vpn`; must match how the goldens were captured (see `regen-golden.sh`) |

## Split-host run

Two boxes, LAN. Box A carries mock + loadgen; Box B carries MCP. Isolates
MCP's CPU/RSS from the mock and the loader.

Box A:
```
./build.sh
CLIENTS=2000 DURATION=60s ./run-loadgen.sh http://<box-b-ip>:9090
```

Box B:
```
./build.sh
MOCK_HOST=<box-a-ip> DURATION=90s ./run-mcp.sh
```

Start Box A first — `run-mcp.sh` refuses to start until Box A's mock is
listening. Once Box A is past the mock startup and waiting for MCP, bring up
Box B; `run-loadgen.sh` then waits up to 5 min for MCP to appear before
firing loadgen.

`run-loadgen.sh` env knobs (full contract in the script header):

| var | default | note |
|---|---|---|
| `CLIENTS` | 200 | `loadgen -clients` — MCP sessions in parallel |
| `DURATION` | 60s | `loadgen -duration` |
| `TOOLS` | `get-broker-status,list-queues` | `loadgen -tools` — subset of tools the mock can answer |
| `BROKERS` | 50 | `loadgen -broker-count` |
| `TOTAL_RPS` | 0 | `loadgen -total-rps` (0 = unlimited); paces aggregate req/s to break the release-barrier convoy |
| `LATENCY_MS` | 0 | `mock-semp -default-latency-ms`; >0 piles requests on MCP's per-broker semaphore |
| `RUN_TAG` | `${CLIENTS}c` | tag appended to the runs dir |
| `NO_MOCK` | 0 | `1` skips starting `mock-semp` (already running elsewhere); error injection is not auto-armed — POST `/_mock/config` yourself |
| `ERROR_RATE` | 0 | probability [0,1] a broker response is injected as an error |
| `ERROR_COUNT` | 0 | cap on injected errors per broker port (0 = unlimited) |
| `ERROR_STATUSES` | `503:70,429:20,500:10` | weighted status pool; only 429/500/502/503/504 accepted |
| `BROKER_ALIAS` | `broker-01` | fidelity `-broker`; must exist in `broker-config.mock.yaml` |
| `VPN` | `vpn_1` | fidelity `-vpn`; must match how the goldens were captured (see `regen-golden.sh`) |

## Fidelity gate

`fidelity` invokes each tool over MCP and deep-equals the result against
`fidelity/golden/*.json` in exact mode. Non-empty diff → exit 1, load run
aborted. The gate runs from both `run.sh` (single-host) and
`run-loadgen.sh` (split-host, on Box A) before error injection is armed,
so a 1% error roll can't flake the pre-run check.

Values in `fidelity/exclusions.txt` (dotted paths, `#` comments) are
skipped by exact-mode diff — currently three fields that advance or
jitter between the canned and golden captures (broker uptime, memory
usage percent). Everything else must match byte-for-byte. Add a path
here only when regen-golden.sh's coordinated recapture shows it truly
drifts within the sub-second window between the two captures.

### Regenerating the fixtures

Neither fixture set is in git — they are captures from a real lab appliance,
and this repo is open source. `.gitignore` excludes `mock-semp/canned/*.json`,
`mock-semp/canned/*.xml`, and `fidelity/golden/*.json`; `mock-semp` reads
`canned/` from disk at startup rather than `go:embed`, so a checkout without
fixtures still builds and `make check` is unaffected. Only this suite needs
them, and only at run time.

`regen-golden.sh` captures both sets in one pass against the real broker.
Doing them together matters: self-changing fields (uptime, memory percentages,
disk usage) drift with wall-clock time, so canned and goldens taken hours
apart cannot match exact-mode comparison even when replaying the same data.
It finishes by writing `fixtures.manifest` — a sha256 per file plus the
capture time, broker alias, and VPN.

`run.sh` and `run-loadgen.sh` verify that manifest before starting anything
(`./fixtures-manifest.sh check`, runnable on its own). The check fails when:

| condition | what it means |
|---|---|
| no manifest | this checkout has never captured — run `regen-golden.sh` |
| a recorded file is missing | partial or half-deleted capture |
| a hash moved | the file was hand-edited after capture |
| an unrecorded fixture is present | a stray mixed in from a different capture |

Age is reported on every run but never blocks it: a hash-clean capture is
internally consistent, which is what the gate depends on. Past
`FIXTURE_AGE_WARN` (default `7d`) the preflight prints a warning that the
broker has moved on since. Under `NO_MOCK=1` the mock lives on another host,
so `run-loadgen.sh` checks the goldens only.

Consequences worth internalizing before you touch a fixture:

- **Don't hand-edit canned or golden files.** A field tweaked by hand in one
  set and not the other fails the gate; tweaked in both, it silently stops
  representing what the broker actually returns.
- **Don't refresh one set alone.** New canned data with old goldens (or the
  reverse) is a guaranteed non-empty diff.
- **Don't cherry-pick from an older capture.** Even the same tool against the
  same broker carries a different uptime and memory reading minutes later.
- **Don't add to `fidelity/exclusions.txt` to paper over a diff.** A path
  belongs there only if a coordinated recapture shows it drifts inside the
  sub-second window between the two captures.

```
CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
```

Overrides: `BROKER_ALIAS` (default `my-broker`), `VPN` (default `default`).
For example, to capture from a non-default VPN:

```
VPN=my-vpn CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
```

Credentials and the broker URL come from the repo-root `.env` (sourced by
the script) via `${BROKER_URL}`, `${BROKER_USERNAME}`, `${BROKER_PASSWORD}`
expansion in the config; override inline if `.env` is absent:

```
BROKER_URL=http://<lab-host>:80 \
  BROKER_USERNAME=... BROKER_PASSWORD=... \
  CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
```

## Sanitization

Keeping the captures out of git is the first line of defence; scrubbing them
is the second, so a fixture that does escape the working tree — pasted into a
ticket, attached to a run report — carries no lab-appliance identity. Every
capture is passed through `mock-semp/canned/sanitize.sh`: chassis/board/disk/
blade serials, MAC addresses, WWPN/WWNN pairs, lab IPs, and the non-GA build
string are replaced with synthetic placeholders (`TESTSERIAL-*`, RFC 7042
documentation MACs, TEST-NET-2 IPs from RFC 5737, a plausible GA-form version
string).

`regen-golden.sh` invokes `sanitize.sh` after the fidelity `-capture` step and
before the manifest is written, so the recorded hashes are of the scrubbed
bytes and a later check doesn't read sanitization as a hand-edit. The script is
idempotent — an already-scrubbed tree passes through as a no-op — and drives
from a single literal-value → replacement table so canned and golden stay
consistent by construction (an inconsistent substitution would make the
exact-mode gate fail).

Editing the placeholder set: update the `subs` array at the top of
`sanitize.sh`, re-run it against the current fixtures, then
`./fixtures-manifest.sh write` to re-record the changed bytes (otherwise the
preflight reports your substitution as a hand-edit), and rerun `./run.sh` to
confirm the fidelity gate still passes.

`broker-config.real.yaml` references `${BROKER_URL}` for the same reason —
no lab address lives in the repo. Set `BROKER_URL` in the environment (or
`.env`) alongside credentials before running `regen-golden.sh`.

## Injecting errors

`ERROR_RATE`, `ERROR_COUNT`, `ERROR_STATUSES` in `run.sh` / `run-loadgen.sh`
exercise MCP's retry/backoff chain. Injection is armed *after* the fidelity
gate, so pre-run checks aren't flaky. Only retryable status codes are
accepted (429/500/502/503/504). Under `NO_MOCK=1`, arm injection yourself by
POSTing to `http://<mock-host>:19000/_mock/config`.

## Miss detection

`mock-semp` treats any unmatched request as a hard failure: logs the miss
and exits non-zero on shutdown. The wrapper scripts propagate that exit
code, so an unrecognized SEMP call fails the run instead of hiding in a log
file. If you extend the tool set, teach `capture.sh` to record the new
response, update the handler, and re-run `regen-golden.sh` so the new fixture
arrives from the same capture as the rest.
