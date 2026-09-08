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
memsampler/       polls /proc/<pid>/status + /proc/<pid>/fd, writes CSV
sampler.sh        CPU + RSS/PSS/USS for MCP + mock + box totals, CSV
loadgen-sampler.sh   loadgen-side connection/goroutine counters
summary.sh        prints a one-page rollup of a run directory
lib.sh            shared helpers: run record, load-phase stamps, port waits,
                  descriptor limit (sourced by the run scripts, never run)

broker-config.mock.yaml   MCP config pointing at mock-semp (50 brokers)
broker-config.real.yaml   MCP config pointing at the real lab broker

gen-mock-config.sh      generates an MCP config for N mock brokers (N > 50)
gen-mock-config.test.sh self-test for the generator
lib.test.sh             self-test for lib.sh + summary.sh's rollup

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

Artifacts land in `bin/runs/<timestamp>[-<tag>]/`, including a `run-record.*`
per role — see [The run record](#the-run-record).

## Ports

| port | who | notes |
|---|---|---|
| `9090` | MCP server | health at `/health`; the run scripts wait up to `PORT_WAIT_SECS` (default 60) for a previous run to release it, then fail naming it |
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
| `BROKERS` | 50 | mock broker count and `loadgen -broker-count`. Above 50 the committed config runs out of aliases — generate one with `gen-mock-config.sh` |
| `PORT_WAIT_SECS` | 60 | how long to wait for a port a previous run still holds |
| `NOFILE` | 1048576 | descriptor limit to request; falls back to the hard limit. Both requested and granted are recorded |
| `RIG_NOTE` | — | free-text note about this host, recorded verbatim in the run record |

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
| `PORT_WAIT_SECS` | 60 | how long to wait for a mock port (or `:19000`) a previous run still holds |
| `NOFILE` | 1048576 | descriptor limit to request; falls back to the hard limit. This is the box that needs it — one outbound socket per `CLIENTS` session |
| `RIG_NOTE` | — | free-text note about this host, recorded verbatim in the run record |

`run-mcp.sh` (Box B) takes `PORT_WAIT_SECS`, `NOFILE` and `RIG_NOTE` too, with
the same meanings and defaults, alongside its own `MOCK_HOST`, `DURATION` and
`CONFIG_FILE` — full contract in the script header. `RIG_NOTE` matters most
there: Box B is the rig whose CPU and RSS a campaign actually compares.

## Running the self-tests

None of these needs a broker, a server, fixtures or a network — they run in a
`mktemp` dir in a couple of seconds:

```
./lib.test.sh              # lib.sh: run record, _source labels, port wait,
                           # and summary.sh's load-phase windowing
./gen-mock-config.test.sh  # the N-broker config generator
go test ./memsampler/      # the /proc parse and the descriptor count
```

`lib.test.sh` is the one that covers the numbers a campaign is compared on. Its
sampler fixture is deliberately bimodal — six idle samples, then six loaded
ones — so a windowing bug cannot pass by accident: the whole-run and
load-phase averages sit 25 points apart, and one assertion requires them to
stay that way.

## The run record

Every run directory gets a machine-readable record of what produced it, one
per role:

```
run-record.mcp        the box that ran MCP        (run.sh, run-mcp.sh)
run-record.loadgen    the box that drove the load (run.sh, run-loadgen.sh)
```

One record per box, never a merged one. The two halves of a split-host run
have different rigs — Box A carries the mock and the load generator, Box B the
MCP server — and merging them would need a channel between the boxes that this
harness deliberately does not have. `run.sh` writes both into its single run
directory, so a consumer reads a single-host run the same way it reads the two
halves of a split-host one.

Format is one `key=value` per line with `#` comments, the same shape the
`.info` sidecars use and `summary.sh` already parses with `awk -F=`. It carries:

| group | fields |
|---|---|
| rig | `host`, `kernel`, `arch`, `cores_logical`, `cores_physical`, `cpu_model`, `mem_total_kb`, plus `instance_type` / `availability_zone` on EC2 and `rig_note` when `RIG_NOTE` is set |
| code | `commit`, `commit_dirty`, and a `<binary>_sha256` for every binary the run executed |
| fixtures | `fixtures_manifest_sha256`, `fixtures_files`, `fixtures_captured_at`, `fixtures_capture_commit`, `fixtures_capture_dirty`, `fixtures_vpn`, `fixtures_rdp`, `fixtures_broker_alias` |
| admission | `semp_max_concurrent_per_broker`, `semp_request_min_interval`, `semp_max_queue_wait`, `semp_fair_scheduling`, each with a `_source` |
| descriptors | `nofile_requested`, `nofile_granted`, `nofile_effective_soft`, `nofile_effective_hard`, `fd_peak`, `threads_peak` |
| workload | `clients`, `duration`, `tools`, `broker_count`, `vpn`, `rdp`, `latency_ms`, `total_rps`, the error-injection knobs |
| load phase | `load_start_epoch`, `load_end_epoch`, `load_window_source` |

The governing rule is that **an absent field is honest and a guessed one is a
trap**. Anything the harness cannot establish is either omitted (EC2 fields on
a devserver: the metadata call simply fails there, which is the expected answer
for "not EC2", not an error) or written as the literal `unknown`. A later
comparison will trust whatever is written here.

One formatting note, since the record is meant to be read with `awk -F=`: a
value containing `=` would read back truncated at the first one, so `=` is
substituted with `:` on write and the substitution is reported on stderr. In
practice this only ever affects `RIG_NOTE`, the one free-text field.

> **These records are internal.** The fixture fields name a real lab appliance
> — `fixtures_vpn`, `fixtures_rdp` and `fixtures_broker_alias` are copied
> straight out of `fixtures.manifest`, which is gitignored for exactly that
> reason — and the rig fields name the host. The records live under the
> gitignored `bin/runs/`, so nothing commits them, but they are meant to be
> shared between engineers and archived: do not paste one into a public issue,
> PR or page. Each record repeats this in its own header so a copied file
> carries the warning with it.

### Why the admission settings carry a `_source`

Four settings move admission behaviour, and two of them post-date the first
full measurement pass, so a run that does not record them cannot be compared
with one that does: `semp.max_queue_wait` and `semp.fair_scheduling`.

Recording them is not as simple as parsing the config, because **unset is not
off**. The defaults are a 100 ms pacer, 10 in-flight slots, a 30 s admission
bound, and fair scheduling **on**. A plain YAML parse of a config that relies
on any of those would leave the field blank, which the next reader takes for
"no throttle".

So each field says where its value came from:

| `_source` | meaning |
|---|---|
| `server-log` | the server reported its own effective value on its `config loaded` startup line |
| `config-file` | the value was written explicitly in the config this run used (copied into the run directory as `broker-config.used.yaml`) |
| `unreported-server-default` | not written down and not reported, so the server applied its default and the harness cannot prove which. The value reads `unknown` |
| `server-log-absent` | the server never logged its `config loaded` line — it did not start, or it is an older build. Value reads `unknown` |
| `server-log-schema-changed` | the line is there but the field is not on it. The harness's one coupling to the server's log output has broken and `lib.sh`'s reader needs updating; it warns on stderr as well. Value reads `unknown` |
| `config-file-unparsed` | the key *is* in the config but `lib.sh`'s narrow reader could not extract it (a nesting or flow-style shape it does not handle). Value reads `unknown`, and it warns — recording `unreported-server-default` here would be a lie about provenance, which is in the same family as a wrong value |

The last three exist so that a broken reader is distinguishable from an absent
value. Collapsing them into one `unknown` would let a server log-schema change
silently degrade every future run with no signal — and the whole reason this
field is read from the log is that it *is* obtainable.

Today only `fair_scheduling` reads `server-log` — it is published there
deliberately, as a kill switch an operator has to be able to confirm took
effect. The other three read `config-file` for any config that sets them (the
committed `broker-config.mock.yaml` sets two of the three) and
`unreported-server-default` otherwise. Putting all four on the `config loaded`
line is a one-line server change that would make every run read `server-log`;
it is production surface, so it is not in this harness.

### The pacer setting changed with this suite

`broker-config.mock.yaml` now sets `semp.request_min_interval: 0s`. It
previously set `1ms` under a comment claiming the pacer was disabled, which was
wrong: only `0s` disables it — `NewRateLimiter` returns a closed channel for a
non-positive interval and builds no ticker, whereas **any** positive value
builds a real ticker per broker. At `1ms` every broker carried a 1000 req/s
admission ceiling that nobody knew was there.

Two consequences worth knowing before comparing anything:

- Throughput figures measured **before** this change were taken with that
  1 ms per-broker pacer in place, and are not directly comparable with figures
  taken after it. Those older runs also predate the run record, so nothing in
  their artifacts reveals the setting — the only way to know is the commit they
  were taken at.
- The measured cost of `1ms` versus `0s` under load is below the noise floor,
  so this is a correctness-of-description fix rather than a performance one.
  Do not expect the numbers to move much; expect them to be *describable*.

## Reading CPU: whole run vs load phase

`summary.sh` reports MCP CPU twice:

```
  mcp   cpu:  min=  1.0%   avg= 26.8%   max= 55.0%   (out of 100% box)
  mcp   cpu:  load-phase  avg= 52.5%   max= 55.0%   (6 of 12 samples, 10:33:50..10:34:15)
```

**Use the load-phase figure.** The whole-run average is diluted by however long
the server sat idle before the load started — the fidelity gate, and in a
split-host run the wait for the other box. That dilution is unstated and
varies per run, so the whole-run average of two runs is not a comparison of
anything. It is kept because older run directories only have that number.

The window is *stamped*, not inferred: the runner that starts the load writes
`load_start_epoch` and `load_end_epoch` into the record, and `summary.sh`
windows the sampler CSV on `sampler.csv`'s `epoch` column. Inferring the
boundary from a CPU threshold would use the metric to define the window it is
measured over, and would break on exactly the runs that matter — an idle-pacer
arm and a CPU-saturated arm look nothing alike.

The split-host **MCP box cannot stamp its own window**: the load runs on the
other box and the two share no channel. Its record says so
(`load_window_source=split-host-load-on-other-box`) and its summary reports the
whole-run figure only. `run-loadgen.sh` prints the command to window it after
the fact, and both run directories are archived together anyway:

```
./summary.sh <box-b-run-dir> --window-from <box-a-run-dir>
```

`--window-from` takes the load box's run directory (or a path straight to its
record), reads the window out of it, and names that record in the report — so a
windowed figure in an archived summary stays traceable to the run that measured
the window. A bare `<from_epoch> <to_epoch>` pair still works and is marked
`unverified` in the output, because a mistyped epoch otherwise yields a
plausible number rather than an error.

## More than 50 brokers

`broker-config.mock.yaml` hand-lists 50 aliases. `mock-semp -listen-count` and
`loadgen -broker-count` both already scale well past that, so the config was
the only thing capping the broker count. Generate one:

```
./gen-mock-config.sh -n 200 -o broker-config.gen200.yaml

# split-host
Box A:  BROKERS=200 ./run-loadgen.sh http://<box-b>:9090
Box B:  CONFIG_FILE=./broker-config.gen200.yaml MOCK_HOST=<box-a> ./run-mcp.sh

# single host
BROKERS=200 CONFIG_FILE=./broker-config.gen200.yaml ./run.sh
```

The generated aliases are byte-identical to the ones `loadgen` generates —
it builds `<prefix>-%02d`, which is a *minimum* width, so `broker-09` and
`broker-200` both come out right and a "tidier" `%03d` would rename all 99 of
the first brokers. That mismatch does not fail loudly: every call 404s at the
mock and the run reads like a server fault. `gen-mock-config.test.sh` asserts
it at one, two and three digits, checks the three `${...}` placeholders survive
verbatim (MCP hard-fails on an unset one), and anchors the whole output shape
against the committed 50-broker config, which it must reproduce byte for byte.

Generated configs are gitignored (`broker-config.gen*.yaml`) and the generator
refuses to write over the committed `broker-config.mock.yaml`.

## Descriptor limits

The run scripts raise `RLIMIT_NOFILE` to a flat generous value (`NOFILE`,
default 1048576, falling back to the hard limit) before launching anything, and
record what was requested, what the shell was granted, and what the server
process itself ended up with — read from `/proc/<pid>/limits`, not from the
launching shell, because the two diverge across a re-exec and the process's own
view is the one that constrains the run.

Nothing in this suite, `cmd/` or `deploy/` raised it before, which left the
ceiling varying with whatever the operator's login shell handed it: a
result-moving input that was invisible in the output.

Where the descriptors go:

- **MCP box.** Each broker gets two protocol clients, each with a transport
  sized `MaxConnsPerHost` = `MaxIdleConnsPerHost` = `max_concurrent_per_broker`.
  In-flight is capped per broker by the shared semaphore, so concurrent sockets
  stay at or below the cap — but the two idle pools are separate and each holds
  up to the cap for `IdleConnTimeout` (90 s), so open descriptors can reach
  *twice* the cap per broker under a protocol-alternating workload.
- **Load box.** Every `loadgen` session is one socket, which dominates a
  2000-caller run. That is a property of the rig, not of the product, which is
  why the two boxes keep separate records.

The limit is deliberately flat rather than computed from the broker count and
the concurrency cap: the arithmetic above is fragile and the descriptors are
free. `memsampler` records the peak count actually reached (`fd_peak` in the
record, `open_fds` per sample in `mem.csv`), so a run that came close to its
ceiling is visible afterwards instead of being a mystery.

### Sampler columns

`mem.csv`, from `memsampler`, one row per second against the MCP process:

```
t_sec, wall_ts, rss_kb, vm_kb, threads, open_fds
```

`open_fds` counts the entries in `/proc/<pid>/fd`, and reads `NA` when that
directory could not be read (another user's process, or the process exiting
between the two reads) — `NA` rather than `0`, because "no descriptors" and "we
could not look" are different facts and a `0` in a run record would be believed.

Descriptors are sampled from `/proc` and **not** scraped off `/metrics`, for two
reasons, the second being the stronger: the Go runtime and process collectors on
`/metrics` carry no scrape test or golden-file exclusion yet, and collecting
them at all requires running with `OBS_METRICS_ENABLED` on — which changes the
thing under test (a histogram observation per tool invocation, plus a scrape
listener) and makes the numbers non-comparable with every run measured so far,
all of which had it off. Reading `/proc` costs nothing and perturbs nothing.
What it does not give is the goroutine count and GC internals; the soak's pass
criteria (RSS drift, threads flat, descriptors flat) do not need them, and
`GODEBUG=gctrace=1` answers "was that memory collectable garbage?" for free.

`sampler.csv`, from `sampler.sh`, every 5 s, covering both processes plus box
totals:

```
t_sec, wall, mcp_cpu, mcp_cpu_pct_of_box, mcp_rss_kb, mcp_pss_kb, mcp_uss_kb,
mock_cpu, mock_cpu_pct_of_box, mock_rss_kb, mock_pss_kb, mock_uss_kb,
loadavg1, sys_mem_used_kb, epoch
```

`epoch` is the last column and is what `summary.sh` windows on: `t_sec` is
relative to the sampler's own start and `wall` carries no date, so neither can
be compared with a stamp taken by another process. It was appended rather than
inserted, so every existing column index still holds.

There is deliberately no descriptor column in `sampler.csv`. `memsampler` is
already per-process and already carries `threads`, so the count belongs there;
a second one here would be one nobody reconciles with the first.

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
capture time, the commit the capture was taken at (`# capture_commit:`, and
`# capture_dirty:` for whether that tree was clean), the broker alias, the VPN,
and the RDP the capture pinned (`# rdp:`). The run record copies that
provenance forward, so a run can name both the fixture set and the code that
produced it.
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
