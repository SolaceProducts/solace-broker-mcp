# canned/ — captured SEMP responses (not in git)

`mock-semp` replays the files in this directory to impersonate a Solace
broker. They are raw response bodies captured from a **real lab broker**, so
they are deliberately untracked: `.gitignore` excludes every `*.json` and
`*.xml` here. The repo carries only the scripts (`capture.sh`, `sanitize.sh`),
this README, and `sanitize.local.tsv.example` — your own
`sanitize.local.tsv`, which names your appliance's identifiers, is gitignored
too.

A fresh clone therefore has no fixtures, and `mock-semp` refuses to start
until you capture some. That is intended — see
[`../../README.md`](../../README.md) ("Regenerating the fixtures") for why
these must be freshly captured rather than shipped.

## Populating it

From `test/performance/`, against a broker you have access to:

```
CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
```

That captures this directory **and** `fidelity/golden/*.json` in one pass,
scrubs lab identifiers via `sanitize.sh`, and records
`test/performance/fixtures.manifest` so later runs can verify the pair is
intact and came from the same capture.

`capture.sh` in this directory does only the canned half. Calling it on its
own leaves the goldens describing an older broker state, which fails the
exact-mode fidelity gate — use `regen-golden.sh` unless you know precisely
why you want just one side.

## What lands here

| file | source |
|---|---|
| `show_version.xml`, `show_system.xml`, `show_memory.xml`, `show_message_spool.xml` | SEMPv1 `<show>` commands behind `get-broker-status` |
| `show_hardware_details.xml` | SEMPv1, appliance-only path of `get-broker-status` |
| `queues_page<N>.json` | SEMPv2 `list-queues`, one file per page; pages must be contiguous from 1. However many the broker's queue count produces — `sanitize.sh` globs them |
| `rdps_page<N>.json` | SEMPv2 `list-rdps`, same per-page scheme |
| `rdp_object.json`, `rdp_queue_bindings.json`, `rdp_rest_consumers.json` | SEMPv2 `get-rdp-status`, for the single RDP pinned at capture time (`RDP_NAME`, or the first the VPN reports) |

`mock-semp` reads the pinned RDP's name out of `rdp_object.json` rather than
taking it as a flag, so the fixture and the rule cannot name different RDPs. A
request for any other RDP name is a miss: 404, a `MISS` line, and a non-zero
exit at shutdown. `fixtures.manifest` records the name (`# rdp:`), and the run
scripts read it from there to pass `-rdp` to `fidelity` and `loadgen`.

`mock-semp` reads them from disk at startup — no `go:embed`, no rebuild
after a recapture. Anything unmatched is a logged miss and a non-zero exit.
