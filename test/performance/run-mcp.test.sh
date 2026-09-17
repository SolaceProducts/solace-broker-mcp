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
# Three outcomes, not two. rc=0 is "a port is held", rc=1 is "both free", and
# rc=2 is "ss is missing, so I cannot tell" — and on that last host run-mcp.sh
# fails closed before it writes a record, so treating it as all-clear turns an
# unsupported environment into a test failure. lib.test.sh skips there; so does
# this.
port_rc=0
held=$(perf_first_held_port 9090 18081) || port_rc=$?
if (( port_rc == 0 )); then
  echo "port $held is in use — is a run in progress? Not running this test." >&2
  exit 0
elif (( port_rc == 2 )); then
  echo "ss (iproute2) not found, so the ports cannot be checked — skipping." >&2
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

echo "== the durations that cannot work are refused before anything starts"

# Zero is a legitimate WARMUP and never a legitimate DURATION: loadgen rejects
# a non-positive -duration, and `memsampler -duration 0s` means "run until the
# process disappears" — so a zero that got past preflight would hold MCP up
# forever and hang the run at its final wait, after the expensive setup.
rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=0s ./run-mcp.sh >"$work/zero.log" 2>&1 ) || rc=$?
eq "DURATION=0s exits 2 rather than starting a run that cannot end" "$rc" "2"
if grep -q "DURATION must be greater than zero" "$work/zero.log"; then
  ok "and says so, naming the value"
else
  bad "the refusal did not name DURATION"
fi

rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=1m30s ./run-mcp.sh >"$work/compound.log" 2>&1 ) || rc=$?
eq "a compound duration the samplers cannot express is refused too" "$rc" "2"

# The knob Box A sets, which this box cannot learn for itself.
rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=30s WARMUP=nonsense ./run-mcp.sh >"$work/warmup.log" 2>&1 ) || rc=$?
eq "and so is a WARMUP that is not a duration" "$rc" "2"

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

# The wiring, not the helpers. Every provenance field added for SOL-154339 is
# one line in this runner, and lib.test.sh exercises the helpers against
# synthetic pids — so deleting the call from run-mcp.sh left every assertion
# in all three shell suites green. These four close that seam: they fail if a call is dropped,
# or moved above the point where $mcp_pid exists.
[[ -n "$(field gomemlimit_env)" ]] && ok "the record carries gomemlimit_env" \
  || bad "gomemlimit_env is missing — perf_record_runtime_mem is not wired into run-mcp.sh"
[[ -n "$(field cgroup_memory_max)" ]] && ok "and cgroup_memory_max beside it" \
  || bad "cgroup_memory_max is missing — perf_record_runtime_mem is not wired into run-mcp.sh"
# Integer, not merely present: this is the anchor summary.sh converts a
# program-relative gctrace stamp with, and a non-integer silently excluded
# every cycle before summary.sh learned to refuse it.
if [[ "$(field mcp_start_epoch)" =~ ^[0-9]+$ ]]; then
  ok "mcp_start_epoch is recorded as integer seconds"
else
  bad "mcp_start_epoch is missing or not integer seconds: [$(field mcp_start_epoch)]"
fi
[[ -n "$(field log_level_source)" ]] && ok "and the log level carries its source" \
  || bad "log_level_source is missing — the log-volume guard is not wired into run-mcp.sh"

# The stub server logs no `config loaded` line, so the level cannot come from
# the server's own report — it has to fall back to the config the run copied
# into the run directory, and say so. This exercises the whole fallback chain
# through the runner, which no library test can.
eq "the level falls back to the config the run used" "$(field log_level)" "info"
eq "and is labelled as coming from the file, not from the server" \
  "$(field log_level_source)" "config-file"

echo "== a run whose logs cannot fit is refused, and says which it was"

# The refusal itself, not the library predicate underneath it. Making the call
# advisory (`|| true`) left every assertion in all three suites green while the
# ten-hour run still filled the disk — so the wiring is asserted here, where
# the runner actually runs.
#
# PERF_LOG_AVAIL_BYTES is lib.sh's test seam for free space; no test can shrink
# a real volume.
# `timeout` because the assertion is that this run does NOT start: if the
# refusal regresses to advisory, the runner holds for DURATION and the suite
# hangs for ten minutes instead of failing. A test for a refusal has to fail
# fast when the refusal is gone.
rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=600s PERF_LOG_AVAIL_BYTES=1000000 \
    timeout 20 ./run-mcp.sh >"$work/refused.log" 2>&1 ) || rc=$?
eq "a projection over the threshold exits 2" "$rc" "2"
if grep -q "REFUSING" "$work/refused.log"; then
  ok "and says it is refusing rather than failing obscurely"
else
  bad "the refusal did not print REFUSING"
  sed -n '1,20p' "$work/refused.log"
fi
if grep -q "ALLOW_VERBOSE_LOGS=1" "$work/refused.log"; then
  ok "and names the override"
else
  bad "the refusal did not name ALLOW_VERBOSE_LOGS"
fi

refrec=$(find "$work/bin/runs" -name 'run-record.mcp' 2>/dev/null | sort | tail -1)
if [[ -n "$refrec" ]]; then
  rfield() { awk -F= -v k="$1" '$1 == k {print $2; exit}' "$refrec"; }
  eq "the record says the run was refused, not interrupted" "$(rfield run_refused)" "log-volume"
  eq "and marks the peak source accordingly" "$(rfield fd_peak_source)" "refused"
  eq "run_terminated is NOT set — it never began" "$(rfield run_terminated)" ""
else
  bad "the refused run wrote no record"
fi

# The override has to actually override, or the knob is decoration.
rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=600s PERF_LOG_AVAIL_BYTES=1000000 \
    ALLOW_VERBOSE_LOGS=1 timeout 12 ./run-mcp.sh >"$work/override.log" 2>&1 ) || rc=$?
if grep -q "running anyway" "$work/override.log"; then
  ok "ALLOW_VERBOSE_LOGS=1 proceeds past the same projection"
else
  bad "ALLOW_VERBOSE_LOGS=1 did not proceed"
  sed -n '1,20p' "$work/override.log"
fi
eq "and the run is not refused with 2" "$([[ "$rc" == 2 ]] && echo refused || echo proceeded)" "proceeded"

echo "== GODEBUG is filtered before it reaches an archived log"

# gctrace is the one value this harness asks for. http2debug=2 writes the SEMP
# client's Authorization header into the same captured mcp.log, which is
# archived and shared — so the runner strips it rather than trusting prose.
rc=0
( cd "$work" && MOCK_HOST=127.0.0.1 DURATION=600s PERF_LOG_AVAIL_BYTES=1000000 \
    GODEBUG=gctrace=1,http2debug=2 timeout 20 ./run-mcp.sh >"$work/godebug.log" 2>&1 ) || rc=$?
if grep -q "GODEBUG reduced to 'gctrace=1'" "$work/godebug.log"; then
  ok "a non-gctrace GODEBUG key is dropped, and the drop is reported"
else
  bad "http2debug was not stripped from GODEBUG"
  sed -n '1,10p' "$work/godebug.log"
fi

echo
echo "$pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
