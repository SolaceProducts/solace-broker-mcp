#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# Self-test for run-mcp.sh's cleanup path — the one part of the run record that
# cannot be tested by calling a function.
#
# What it pins: a run that is terminated rather than allowed to finish still
# writes fd_peak and threads_peak, writes them exactly once despite cleanup
# being the handler for EXIT, INT and TERM alike, and marks the peaks as
# partial so a short run cannot be quoted as a whole one. All three regressed
# invisibly before — the peaks were written in the main flow, so any terminated
# run simply lost them, and the run whose entire purpose was measuring fd_peak
# under a raised connection cap came back without it.
#
# Everything real is stubbed: a Python listener stands in for MCP's /health, a
# shell loop for memsampler, a bare socket for the mock on the other box. No
# broker, no fixtures, no network beyond loopback.
#
# Usage: ./run-mcp.test.sh

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"

pass=0
fail=0
ok()  { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
eq()  { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1"$'\n'"        got:  [$2]"$'\n'"        want: [$3]"; fi; }

# The stub MCP binds 9090 and the stub mock 18081, the same ports a real run
# uses. Bail out rather than fight a run already in progress — a test that
# killed someone's campaign to prove a point would be a poor trade.
if held=$(perf_first_held_port 9090 18081); then
  echo "port $held is in use — is a run in progress? Not running this test." >&2
  exit 0
fi

tmp="$(mktemp -d)"
work="$tmp/perf"
mkdir -p "$work/bin"
# The runner reads its neighbours by path, so it needs a directory that looks
# like test/performance — but only the files it actually sources or execs.
cp "$here/run-mcp.sh" "$here/lib.sh" "$here/sampler.sh" "$here/summary.sh" "$work/"
printf 'stub — this run replays nothing\n' >"$work/fixtures.manifest"
cp "$here/broker-config.mock.yaml" "$work/" 2>/dev/null || printf 'brokers: []\n' >"$work/broker-config.mock.yaml"

cat >"$work/bin/mcp-server" <<'EOF'
#!/usr/bin/env python3
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("", 9090), H).serve_forever()
EOF
# Appends a row a second and flushes each one, which is what makes a peak
# readable from a file whose writer was killed mid-run.
cat >"$work/bin/memsampler" <<'EOF'
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do [[ "$1" == "-out" ]] && out=$2; shift; done
echo "t_sec,wall_ts,rss_kb,vm_kb,threads,open_fds" >"$out"
for i in $(seq 1 600); do
  echo "$i,x,1000,2000,$((40 + i)),$((500 + i))" >>"$out"
  sleep 1
done
EOF
chmod +x "$work/bin/mcp-server" "$work/bin/memsampler"

cleanup_all() {
  [[ -n "${mock_pid:-}" ]] && kill "$mock_pid" 2>/dev/null
  [[ -n "${run_pid:-}" ]]  && kill -KILL "$run_pid" 2>/dev/null
  pkill -f "$work/bin/" 2>/dev/null
  rm -rf "$tmp"
  return 0
}
trap cleanup_all EXIT

# Stands in for mock-semp on Box A: run-mcp.sh refuses to start MCP until
# something is listening there, which is itself worth not breaking.
python3 -c "
import socketserver
class H(socketserver.BaseRequestHandler):
    def handle(self): self.request.recv(1024)
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(('127.0.0.1', 18081), H).serve_forever()" &
mock_pid=$!
sleep 1

echo "== a run terminated mid-hold still describes itself"

# Two things this launch has to get right, both of them learned the hard way:
#
#   `exec`  — without it $! is the subshell's pid, a SIGTERM there never
#             reaches the script, and the trap this test exists to exercise
#             does not run.
#   `setsid`— the runner's own kill_tree signals its *process group*, so a
#             runner sharing this test's group takes the test down with it
#             mid-assertion. Its own session keeps the blast radius where it
#             belongs. (That kill_tree reaches its own group is pre-existing
#             behaviour, not something this test is asserting.)
( cd "$work" && exec setsid env MOCK_HOST=127.0.0.1 DURATION=120s ./run-mcp.sh >"$work/run.log" 2>&1 ) &
run_pid=$!
# Long enough for the health check, the record, and several sampler rows.
sleep 14

if ! kill -TERM "$run_pid" 2>/dev/null; then
  bad "the runner exited before it could be interrupted"
  sed -n '1,15p' "$work/run.log"
  echo; echo "$pass passed, $fail failed"; exit 1
fi
sleep 3

rec=$(find "$work/bin/runs" -name 'run-record.mcp' 2>/dev/null | head -1)
if [[ -z "$rec" ]]; then
  bad "no run record was written at all"
  sed -n '1,15p' "$work/run.log"
  echo; echo "$pass passed, $fail failed"; exit 1
fi
field() { awk -F= -v k="$1" '$1 == k {print $2; exit}' "$rec"; }

# The defect this whole change exists for: these were written in the main flow,
# so a terminated run reached the end of neither and lost both.
[[ -n "$(field fd_peak)" ]]      && ok "fd_peak survives a terminated run"      || bad "fd_peak is missing from a terminated run's record"
[[ -n "$(field threads_peak)" ]] && ok "threads_peak survives a terminated run" || bad "threads_peak is missing from a terminated run's record"

# cleanup() handles EXIT, INT and TERM, and its own `exit` re-enters it through
# the EXIT trap — so without the peaks_recorded guard the record grows a second
# copy of every peak, and `awk -F= ... exit` would read whichever came first.
eq "fd_peak is written exactly once, not once per trap entry" \
  "$(grep -c '^fd_peak=' "$rec")" "1"
eq "threads_peak likewise" "$(grep -c '^threads_peak=' "$rec")" "1"

# A peak over a run that was cut short is not the peak the run would have
# reached. Absence used to be the marker for that; now the record says it.
eq "the record says the run was terminated" "$(field run_terminated)" "true"
eq "and marks the peak partial rather than letting it be quoted as complete" \
  "$(field fd_peak_source)" "partial"

# This box stamps no load window by design, so these fields are the only thing
# separating an interrupted record from a whole one.
eq "the load window is still declared, not guessed" \
  "$(field load_window_source)" "split-host-load-on-other-box"

echo
echo "$pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
