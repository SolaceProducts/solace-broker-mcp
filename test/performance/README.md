# Performance test suite

> **Experimental — not for production use.** Everything in this directory is
> internal test apparatus: a load generator, a mock SEMP server, a fidelity
> differ, and a memory sampler. None of it ships in a release artifact, none of
> it is part of the supported surface of the MCP server, and none of it carries
> the compatibility, security-review, or support guarantees that production
> code does. The binaries here are built to be pointed at throwaway test
> brokers — do not run them against a production broker, and do not build
> anything on top of them. Interfaces, flags, and output formats change without
> notice or changelog entry.
>
> For the same reason these packages are excluded from the repo-wide unit-test
> coverage gate — see
> [docs/internal/unit-test-coverage.md](../../docs/internal/unit-test-coverage.md).

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

build.sh          builds mock-semp, loadgen, fidelity, memsampler, mcp-server into ./bin/
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
| `19000` | mock-semp control endpoint | `POST /_mock/config` for per-port latency / error injection; `GET /_mock/hits` reports per-rule SEMP counts and `POST /_mock/hits` reports and zeroes them. Bound to localhost by default (separate from `-listen-addr`) so opening broker ports to the LAN doesn't also expose the injection knob |

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
| `TOOLS` | all four | `get-broker-status,list-queues,list-rdps,get-rdp-status`; set it to a subset to isolate one tool's cost. Validated in the step-0 preflight, before the mock starts — an unknown tool aborts the run immediately |
| `LATENCY_MS` | 0 | per-response sleep in mock; use to force per-broker semaphore queueing inside MCP |
| `ERROR_RATE` | 0 | probability each broker response is injected as an error |
| `ERROR_COUNT` | 0 | cap on injected errors per broker port (0 = unlimited) |
| `ERROR_STATUSES` | `503:70,429:20,500:10` | weighted status pool; only 429/500/502/503/504 accepted |
| `BROKER_ALIAS` | `broker-01` | fidelity `-broker`; must exist in `broker-config.mock.yaml` |
| `VPN` | from `fixtures.manifest` | fidelity `-vpn`; defaults to the VPN the goldens were captured against. Set it only to override |
| `RDP` | from `fixtures.manifest` | fidelity/loadgen `-rdp`; the RDP the capture pinned. The mock serves `get-rdp-status` for that RDP only |

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
| `TOOLS` | all four | `loadgen -tools`; `get-broker-status,list-queues,list-rdps,get-rdp-status`, or a subset to isolate one tool's cost. Validated in the step-0 preflight, so a typo fails before the mock binds and before the wait for Box B |
| `BROKERS` | 50 | `loadgen -broker-count` |
| `TOTAL_RPS` | 0 | `loadgen -total-rps` (0 = unlimited); paces aggregate req/s to break the release-barrier convoy |
| `LATENCY_MS` | 0 | `mock-semp -default-latency-ms`; >0 piles requests on MCP's per-broker semaphore |
| `RUN_TAG` | `${CLIENTS}c` | tag appended to the runs dir |
| `NO_MOCK` | 0 | `1` skips starting `mock-semp` (already running elsewhere); error injection is not auto-armed — POST `/_mock/config` yourself |
| `ERROR_RATE` | 0 | probability [0,1] a broker response is injected as an error |
| `ERROR_COUNT` | 0 | cap on injected errors per broker port (0 = unlimited) |
| `ERROR_STATUSES` | `503:70,429:20,500:10` | weighted status pool; only 429/500/502/503/504 accepted |
| `BROKER_ALIAS` | `broker-01` | fidelity `-broker`; must exist in `broker-config.mock.yaml` |
| `VPN` | from `fixtures.manifest` | fidelity `-vpn`; defaults to the VPN the goldens were captured against. Set it only to override |
| `RDP` | from `fixtures.manifest` | fidelity/loadgen `-rdp`; the RDP the capture pinned |

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

The gate covers five checks:

| check | arguments |
|---|---|
| `get-broker-status` | broker only |
| `list-queues` | `msgVpnName` |
| `list-rdps` | `msgVpnName` |
| `list-rdps (maxResults=200, paginated)` | `msgVpnName`, `maxResults=200` |
| `get-rdp-status` | `msgVpnName`, `restDeliveryPointName` |

The paginated `list-rdps` check exists because the default `maxResults` of 100
stops `followPages` before it asks for a second page — so the plain check never
exercises pagination however many RDPs the broker holds. Asking for 200 drives
the mock through its cursor rule, and since the golden records every RDP
returned, exact-mode length comparison is what proves they all came back.

That check is the only thing in the suite that walks a cursor chain, and it can
only do so if the capture produced more than one RDP page. A capture from a VPN
with 100 or fewer RDPs still passes it — a one-page golden against a one-page
replay — so the coverage would vanish with nothing going red. Both
`regen-golden.sh` (at capture time, where recapturing from a bigger VPN is still
an option) and `mock-semp` (at every startup) warn when that happens.

### Fixture durability

The gate is exact, but its three exclusions are not evenly distributed, and
that matters when you recapture from a busier broker.

| fixture tool | stays exact on a realistic capture? | why |
|---|---|---|
| `list-rdps` | **yes** | Every field it selects is settled while the RDPs are disabled: `up:false`, `lastFailureTime` frozen at the moment each was shut down rather than advancing on retries. Two reads seconds apart are byte-identical |
| `get-rdp-status` | **yes** | Same, plus `uptime:0` and all HTTP counters at `0` on a disabled REST consumer |
| `list-queues` | **no** | Six of the thirteen fields it selects move under any traffic at all: `bindCount`, `msgSpoolUsage`, `spooledMsgCount`, `rxMsgRate`, `txMsgRate`, `txUnackedMsgCount`. It needs no exclusions today, but that is a property of the capture VPN being idle — not of the harness. On a loaded broker the only way to keep it green is to exclude those six, at which point the check stops verifying most of what the tool returns |
| `get-broker-status` | **no** | Already the source of all three exclusions (uptime twice, memory percent) |

So: realism and strictness are in tension for the two original fixture tools
and not for the RDP pair. If you recapture against a broker under load and the
gate goes red on `list-queues`, that is the expected outcome, not a
regression — and adding those fields to `exclusions.txt` is a decision to stop
checking them, which is worth making deliberately.

### What this fixture cannot tell you

Two limits are structural, not oversights:

- **SEMPv2/JSON only.** `get-redundancy-status` — the one single-call SEMPv1
  tool, and the natural same-cost cross-protocol comparator — is deliberately
  not in the fixture. `get-broker-status` is SEMPv1 but fans out to 4–5 calls
  and its count varies with broker type, so it cannot serve as a clean SEMPv1
  denominator. Any throughput figure from this harness covers the SEMPv2/JSON
  path; the SEMPv1 ceiling is unmeasured.
- **Every RDP in the capture is disabled.** That is exactly right for
  regression detection — nothing drifts — but it means the fixtures never
  exercise a healthy RDP with live counters, and their values are short and
  repetitive, so they are cheap to parse and allocate. A throughput ceiling
  measured against them is an **upper bound, not a sizing figure**.

### SEMP cost per tool call

`loadgen` reports tool calls per second; the performance targets are written in
SEMP requests per second. The conversion factor is each tool's fan-out, and
`mock-semp` measures it directly: every rule counts the requests it served, and
those counts are readable two ways. `GET /_mock/hits` reports them mid-run;
`POST` reports and zeroes them.

The run scripts use that to hand you the table without a calibration run. The
fidelity gate makes exactly one call per check, so step `3b` POSTs `/_mock/hits`
the moment it passes: the gate's own fan-out lands in `semp-fanout.json` beside
the other artifacts, and the counters restart at zero so the shutdown summary in
`mock.log` (`SEMP requests served per rule`) measures the load phase alone. Both
places name the window they cover. Without that reset the two phases share one
tally, which is invisible under load and the whole number in a low-volume run.
The miss count is never reset — it feeds the shutdown gate.

| call | SEMP requests | note |
|---|---|---|
| `list-rdps` (default args) | 1 | reported tool rate **is** the SEMP rate, no conversion |
| `list-rdps maxResults=200` | 2 | page 1 + one cursor follow |
| `list-queues` (default args) | 1 | same 1-call shape |
| `get-rdp-status` | 3 | object, then queue bindings and REST consumers in parallel |
| `get-broker-status` | 5 | SEMPv1; the fifth (`show hardware details`) fires on appliances only, so this is 4 on a software broker |

`semp-fanout.json` is keyed by **rule, not by tool**, and the two `list-rdps`
rows above share rules. The gate calls `list-rdps` twice — once at default
arguments, once at `maxResults=200` — so `sempv2 rdps page 1` reads 2 (one hit
from each) and `sempv2 rdps page 2 (cursor)` reads 1, belonging only to the
paginated call. The totals reconcile with the table (2 + 1 = 1 + 2), but the
per-call numbers are the table's, not the file's. Every other row maps to its
rules one-to-one.

The default `maxResults` is **100**, so `followPages` never paginates at
default arguments regardless of how much data the broker holds — the SEMP cost
of a list tool is set by the caller's `maxResults`, not by broker size.
`loadgen` has no `maxResults` flag, so every list tool in a load run costs
exactly 1 SEMP request.

`TOOLS` defaults to all four, and each client rotates through them evenly, so
the default workload averages `(5 + 1 + 1 + 3) / 4 = 2.5` SEMP requests per
tool call (2.25 against a software broker, where `get-broker-status` is 4).
There is no single conversion factor for a mixed rotation: read the per-rule
counts out of `mock.log` rather than multiplying the reported tool rate. Set
`TOOLS` to one tool when you want a rate that converts by a single number, or
when you want to attribute a latency tail to a specific fan-out.

One consequence worth knowing rather than discovering: a capture of a VPN with
more than 100 queues produces `queues_page2.json` and beyond, and `mock-semp`
registers a cursor rule for each — but nothing in the suite requests them,
because every `list-queues` call it makes uses default arguments. The shutdown
summary shows them as `(never fired)`. Exercising them would mean
adding a paginated `list-queues` check, which changes how `list-queues` is
gated; `list-rdps` carries the paginated check instead, and the cursor
machinery is shared by both collections, so the code path is covered either
way.

`list-rdps` and `get-rdp-status` are in the fixture specifically as a 1-call
and 3-call pair on the same protocol against the same objects in the same VPN,
differing in nothing but fan-out. `get-rdp-status` also contributes two things
no other fixture tool does: concurrent sub-requests inside a single tool call
(its two collection steps are `parallel: true`, so one call holds up to two
per-broker concurrency slots), and a small-payload datapoint — 2,135 bytes of
tool output against `list-rdps`'s 26,274 on the same protocol and VPN, which
separates per-request overhead from per-byte cost.

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
capture time, broker alias, VPN, and the RDP the capture pinned (`# rdp:`).
That last one is not just provenance: `mock-semp` serves `get-rdp-status` for
that RDP alone, so `run.sh` and `run-loadgen.sh` read it back
(`./fixtures-manifest.sh rdp`) to pass `-rdp` to `fidelity` and `loadgen`. A
manifest without it stops both scripts with a pointer to recapture, rather than
letting them ask for an RDP the mock will 404.

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

Overrides: `BROKER_ALIAS` (default `my-broker`), `VPN` (default `default`),
`RDP_NAME` (the single RDP `get-rdp-status` is captured and replayed for —
defaults to the first RDP the VPN reports, derived from the captured
collection). Setting `RDP_NAME` to a name the VPN does not hold fails before
anything is captured; leaving it unset cannot name an RDP the broker lacks.
Prefer an RDP with one queue binding and one REST consumer: `get-rdp-status`
declares no `maxResults`, so a busier one is captured silently truncated at 100
rather than caught.
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
capture is passed through `mock-semp/canned/sanitize.sh`, which works two ways
because the identifiers are not equally detectable:

**A substitution table**, `mock-semp/canned/sanitize.local.tsv`. Literal
value → placeholder, one pair per line. Serials have no recognizable shape, so
naming them is the only option — and naming them in a *tracked* file would
publish exactly what the table exists to remove, so the table is gitignored and
per-checkout. Start from the tracked `sanitize.local.tsv.example`, which
documents the placeholder conventions (`TESTSERIAL-*`, RFC 7042 documentation
MACs, TEST-NET-2 addresses from RFC 5737, a GA-form version string).

**A residual scan** over the scrubbed bytes, which fails the capture when a
mechanically-recognizable identifier survives: a MAC outside the documentation
range, a `192.168`/`172.16` address, an FC WWPN, or a non-GA build marker
(`+lo.NNN`, `NNNmain.N`). It reports file, line, and value. This is what makes
the harness safe on an appliance the table was not written for — that case used
to scrub nothing and say nothing.

> The scan cannot see serials, and `10.0.0.0/8` is deliberately outside it
> (Solace version strings are `10.x.y.z` and would match an IPv4 pattern, so
> scanning that range would fail every capture on its own version attribute).
> **A clean scan is not proof the fixtures are clean.** If your appliance is
> not the one your table describes, write a table for it.

`regen-golden.sh` invokes `sanitize.sh` after the fidelity `-capture` step and
before the manifest is written, so the recorded hashes are of the scrubbed
bytes and a later check doesn't read sanitization as a hand-edit. The script is
idempotent — an already-scrubbed tree passes through as a no-op — and both
fixture sets go through one table, so canned and golden stay consistent by
construction (an inconsistent substitution would fail the exact-mode gate).
It rewrites capture output only, never tracked scripts.

Adding a value: append it to `sanitize.local.tsv`, re-run `./sanitize.sh`, then
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
response, update the handler, add the file to `sanitize.sh`'s list, and re-run
`regen-golden.sh` so the new fixture arrives from the same capture as the rest.

That gate is why `get-rdp-status` is pinned to one RDP by exact path match. The
alternative shape — matching `/restDeliveryPoints/` by prefix — would answer a
request for any of the VPN's 196 RDPs with the one RDP that was captured: a
wrong answer that looks exactly like a right one, and that no other check in
the harness would catch. Compare the five SEMPv1 rules, which all share the
`/SEMP` path and are told apart by first-substring-match on the request body;
that shape has the hazard, and the RDP rules deliberately don't inherit it.

Shutdown (and `/_mock/hits` mid-run) also reports the requests each rule
served, including a `(never fired)` marker. Use it to sanity-check a new rule: a
SEMPv2 rule written against the public `/SEMP/v2/monitor/` path only will
never fire, because MCP uses `/SEMP/v2/__private_monitor__/`.

`sanitize.sh` closes with the RDP endpoint values from the capture
(`remoteHost`, `remotePort`, `postRequestTarget`). Its residual scan matches
192.168/16 and 172.16/12 addresses, so an RDP pointed at a real internal service
*by hostname* passes it; printing those values every run puts the one thing a
clean scan cannot vouch for in front of whoever ran it. Confirm none of them
names something real before the capture is trusted.
