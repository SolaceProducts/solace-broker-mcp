# Performance test suite

Load-test harness for the MCP server. Spins up a fake fleet of Solace brokers
(`mock-semp`), drives concurrent MCP tool calls (`loadgen`), samples MCP and
mock resource use (`memsampler`, `sampler.sh`), and gates every run behind a
byte-for-byte fidelity check against golden captures from the real broker
(`fidelity`).

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
regen-golden.sh   re-capture fidelity/golden/*.json from the real broker
```

Artifacts land in `bin/runs/<timestamp>[-<tag>]/`.

## Quick start (single host)

```
./build.sh
./run.sh                          # 32 clients, 60s
CLIENTS=200 DURATION=2m ./run.sh
```

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

## Fidelity gate

`fidelity` invokes each tool over MCP and deep-equals the result against
`fidelity/golden/*.json`. Non-empty diff → exit 1, load run aborted.

Regenerate goldens after a legitimate tool-shape change:

```
BROKER_USERNAME=... BROKER_PASSWORD=... ./regen-golden.sh
```

This starts MCP against the real broker (`broker-config.real.yaml` or your
`broker-config.yaml`), captures fresh JSON, then tears down. Review the diff
before committing.

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
file. If you extend the tool set, add a canned response under
`mock-semp/canned/` and update the handler.
