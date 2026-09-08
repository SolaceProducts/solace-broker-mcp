#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# Self-test for lib.sh and summary.sh's rollup.
#
# These two files decide the numbers a measurement campaign is compared on, and
# almost all of that logic is awk over a file. The dangerous failure is not a
# crash: a field-index slip in the load-phase window degrades to "no samples
# inside the stamped window" and a mis-sourced admission setting writes a
# confident `config-file` over a value nobody wrote down. Both produce a report
# that looks right.
#
# So the assertions here are on the two things a later comparison actually
# trusts: the load-phase figure, and the `_source` label next to each setting.
#
# Every function under test is pure over a file or a string, so the fixtures
# are synthetic and the whole thing runs in a mktemp dir with no broker, no
# server and no network.
#
# Usage: ./lib.test.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0
skip=0

ok()   { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
# Counted, not just printed. A section that skips silently looks like coverage
# in CI output, which is worse than a visible gap.
skip() { printf '  skip  %s\n' "$1"; skip=$((skip + 1)); }

# indent <file> [max] — awk, not `sed | head`: a head that closes the pipe
# early SIGPIPEs its producer, and under `pipefail` that aborts this script, so
# the first real failure would kill the report instead of printing it.
indent() { awk -v max="${2:-12}" 'NR <= max { print "        " $0 }' "$1"; }

eq() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1"$'\n'"        got:  [$2]"$'\n'"        want: [$3]"; fi
}

contains() { # <name> <haystack> <needle>
  if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1 (missing: $3)"$'\n'"$(printf '%s\n' "$2" | awk 'NR<=8{print "        " $0}')"; fi
}

lacks() { # <name> <haystack> <needle>
  if [[ "$2" != *"$3"* ]]; then ok "$1"; else bad "$1 (unexpectedly present: $3)"; fi
}

# --- fixtures ---------------------------------------------------------------

# A sampler CSV with a deliberately bimodal shape: six idle samples at ~1% of
# box, then six loaded ones at 50-55%. The whole-run average lands near 26%,
# which is the diluted figure item 1 of the story exists to stop people
# quoting, and the load-phase average lands near 52% — so a windowing bug
# cannot pass by accident, because the two numbers are far apart.
BASE=1788878000
make_sampler_csv() { # <path> [--no-epoch]
  local out=$1 no_epoch=${2:-}
  {
    if [[ "$no_epoch" == "--no-epoch" ]]; then
      echo "t_sec,wall,mcp_cpu,mcp_cpu_pct_of_box,mcp_rss_kb,mcp_pss_kb,mcp_uss_kb,mock_cpu,mock_cpu_pct_of_box,mock_rss_kb,mock_pss_kb,mock_uss_kb,loadavg1,sys_mem_used_kb"
    else
      echo "t_sec,wall,mcp_cpu,mcp_cpu_pct_of_box,mcp_rss_kb,mcp_pss_kb,mcp_uss_kb,mock_cpu,mock_cpu_pct_of_box,mock_rss_kb,mock_pss_kb,mock_uss_kb,loadavg1,sys_mem_used_kb,epoch"
    fi
    local i e row
    for i in $(seq 0 11); do
      e=$((BASE + i * 5))
      if (( i < 6 )); then
        row="$((i*5)),$(date -d "@$e" +%H:%M:%S),16.0,1.0,80000,70000,60000,NA,NA,NA,NA,NA,0.5,900000"
      else
        # The loaded samples deliberately differ from each other. With all six
        # identical, the load-phase *average* was insensitive to which of them
        # fell inside the window, so a slipped inclusive bound was caught only
        # by the sample count. These six average to exactly 51.0, and any
        # off-by-one at either edge moves that number.
        box=$(awk -v k="$i" 'BEGIN { split("46 48 50 52 54 56", v, " "); print v[k-5] ".0" }')
        row="$((i*5)),$(date -d "@$e" +%H:%M:%S),8$i.0,$box,190000,180000,170000,NA,NA,NA,NA,NA,7.5,1200000"
      fi
      [[ "$no_epoch" == "--no-epoch" ]] && echo "$row" || echo "$row,$e"
    done
  } >"$out"
  printf 'host=fixture\ncores_logical=16\nmem_total_kb=16777216\nmode=mcp-only\n' >"$out.info"
}

# --- summary.sh: the load-phase window ---------------------------------------

echo "== summary.sh windows CPU on the stamped load phase"

run_a="$tmp/run-a"
mkdir -p "$run_a"
make_sampler_csv "$run_a/sampler.csv"
{
  echo "record_version=1"
  echo "role=mcp"
  echo "started_at=2026-09-08T10:33:20-04:00"
  # Cover exactly the six loaded samples: t=30s..55s.
  echo "load_start_epoch=$((BASE + 30))"
  echo "load_end_epoch=$((BASE + 55))"
} >"$run_a/run-record.mcp"

out=$("$here/summary.sh" "$run_a")

contains "the whole-run average is still reported, and is the diluted one" \
  "$out" "avg= 26.0%"
# Anchored to the load-phase line as a whole, not to a bare "max=" needle: the
# whole-run line above also prints a max, so a needle of "max= 56.0%" alone
# matched the wrong line and survived deleting the load-phase max entirely.
contains "a labelled load-phase mean AND maximum are reported alongside it" \
  "$out" "load-phase  avg= 51.0%   max= 56.0%"
contains "the load-phase line says how many samples fell inside the window" \
  "$out" "(6 of 12 samples"
contains "the window's source record is named, so an archived report is traceable" \
  "$out" "window from $run_a/run-record.mcp"
contains "the source record's role and start time are named" \
  "$out" "role=mcp"

# The point of the whole change: the two figures must differ, and the
# load-phase one must be the higher. If a future edit silently windows on
# everything, this is the assertion that catches it.
# Pull the two averages by pattern, not by field index: the report pads with
# multiple spaces ("min=  1.0%"), so awk splits "avg=" and its value into
# separate fields and a positional read lands on the wrong one.
# awk over a here-string, not `printf | sed …;q`: a sed that quits early
# SIGPIPEs printf, and under `pipefail` that aborts this script inside an
# assignment — the same hazard indent() above exists to avoid. Latent only
# because the report currently fits the pipe buffer.
avg_of() { # <line-pattern>
  awk -v pat="$1" 'index($0, pat) && match($0, /avg=[ ]*[0-9.]+%/) {
    v = substr($0, RSTART, RLENGTH); gsub(/avg=|[ ]|%/, "", v); print v; exit
  }' <<<"$out"
}
whole=$(avg_of 'mcp   cpu:  min=')
phase=$(avg_of 'load-phase')
if awk -v w="$whole" -v p="$phase" 'BEGIN { exit !(p > w + 10) }'; then
  ok "the load-phase figure is materially higher than the diluted whole-run one ($phase% vs $whole%)"
else
  bad "load-phase ($phase%) is not materially above whole-run ($whole%) — is the window being applied?"
fi

echo "== summary.sh degrades honestly when it cannot window"

run_b="$tmp/run-b"
mkdir -p "$run_b"
make_sampler_csv "$run_b/sampler.csv"
out=$("$here/summary.sh" "$run_b")
contains "no run record: says the window is not stamped" "$out" "load phase: not stamped"
lacks    "no run record: prints no load-phase figure"    "$out" "load-phase  avg="
contains "no run record: the whole-run figure is unchanged" "$out" "avg= 26.0%"

# A run directory from before this change: 14 columns, no epoch. It must read
# exactly as it did before, and must NOT sprout a load-phase line even when a
# window is supplied — there is no column to window on.
run_c="$tmp/run-c"
mkdir -p "$run_c"
make_sampler_csv "$run_c/sampler.csv" --no-epoch
out=$("$here/summary.sh" "$run_c" $((BASE + 30)) $((BASE + 55)))
contains "pre-epoch CSV: the whole-run figure still reports" "$out" "avg= 26.0%"
lacks    "pre-epoch CSV: no load-phase line is invented"     "$out" "load-phase  avg="

# A window that misses every sample must say so rather than print a figure
# over zero samples (which would divide by zero or print 0.0%).
run_d="$tmp/run-d"
mkdir -p "$run_d"
make_sampler_csv "$run_d/sampler.csv"
out=$("$here/summary.sh" "$run_d" $((BASE + 9000)) $((BASE + 9100)))
contains "a window matching no samples says so" "$out" "no samples inside the stamped window"
lacks    "a window matching no samples prints no average" "$out" "load-phase  avg="

echo "== summary.sh resolves CSV columns by header name"

# The whole point of resolving by name is that a column's position may move.
# Every fixture above uses the canonical order, so hardcoding bc=4/rc=5/ec=15
# back into roll() would pass all of them — this is the fixture that stops it.
run_e="$tmp/run-e"
mkdir -p "$run_e"
# Same data, header and every row reordered: epoch first, then the mcp columns
# in a different order. The reported numbers must not change.
awk -F, 'BEGIN { OFS="," }
  { print $15, $2, $1, $4, $3, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14 }' \
  "$run_a/sampler.csv" >"$run_e/sampler.csv"
cp "$run_a/sampler.csv.info" "$run_e/sampler.csv.info"
cp "$run_a/run-record.mcp" "$run_e/run-record.mcp"
out=$("$here/summary.sh" "$run_e")
contains "a reordered header still yields the same whole-run figure" "$out" "avg= 26.0%"
contains "a reordered header still yields the same load-phase figure" \
  "$out" "load-phase  avg= 51.0%   max= 56.0%"
contains "and still finds epoch, so the window still applies" "$out" "(6 of 12 samples"
# The window-bounds field is the only part of the load-phase line that says
# WHICH samples were averaged, and it was read positionally long after every
# other column had been converted — so on a reordered header it printed an
# unrelated column. Assert it looks like a wall clock.
if printf '%s\n' "$out" | grep -q 'load-phase.*samples, [0-9][0-9]:[0-9][0-9]:[0-9][0-9]\.\.[0-9][0-9]:[0-9][0-9]:[0-9][0-9])'; then
  ok "a reordered header still reports wall-clock window bounds, not another column"
else
  bad "the load-phase window bounds are not wall-clock times on a reordered header"
  printf '%s\n' "$out" | awk '/load-phase/ { print "        " $0 }'
fi

# A column the report needs but the CSV does not have is a producer/consumer
# mismatch. It must say so, not fall through to "(no samples)", which is a real
# and very different state (an idle process).
run_f="$tmp/run-f"
mkdir -p "$run_f"
sed '1s/mcp_cpu_pct_of_box/mcp_cpu_pct_renamed/' "$run_a/sampler.csv" >"$run_f/sampler.csv"
cp "$run_a/sampler.csv.info" "$run_f/sampler.csv.info"
out=$("$here/summary.sh" "$run_f")
contains "a renamed column is reported as a header mismatch" \
  "$out" "column mcp_cpu_pct_of_box not in this CSV"
lacks    "and is not passed off as an idle process" "$out" "mcp   (no samples)"

echo "== summary.sh's mem.csv block"

run_g="$tmp/run-g"
mkdir -p "$run_g"
cat >"$run_g/sampler.csv" <<'CSV'
t_sec,wall,mcp_cpu,mcp_cpu_pct_of_box,mcp_rss_kb,mcp_pss_kb,mcp_uss_kb,mock_cpu,mock_cpu_pct_of_box,mock_rss_kb,mock_pss_kb,mock_uss_kb,loadavg1,sys_mem_used_kb,epoch
5,10:00:05,16.0,1.0,80000,70000,60000,NA,NA,NA,NA,NA,0.5,900000,1788878005
CSV
printf 'host=fixture\ncores_logical=16\nmem_total_kb=16777216\nmode=mcp-only\n' >"$run_g/sampler.csv.info"
# A trailing unreadable fd sample, which is what a process winding down
# produces. `end=` must report that, not the last readable value.
cat >"$run_g/mem.csv" <<'CSV'
t_sec,wall_ts,rss_kb,vm_kb,threads,open_fds
0.000,x,80000,900000,14,42
1.000,x,190000,900000,58,913
2.000,x,150000,900000,31,NA
CSV
printf 'nofile_effective_soft=1048576\n' >"$run_g/run-record.mcp"
out=$("$here/summary.sh" "$run_g")
contains "mem.csv peaks are reported"              "$out" "peak=913"
contains "thread peak too"                          "$out" "peak=58"
contains "a trailing unreadable fd sample reports end=NA, not a stale value" \
  "$out" "fds:  end=NA"
lacks    "and does not report the last readable sample as the end" "$out" "end=120"
contains "the descriptor limit is shown next to the peak" "$out" "limit=1048576"

echo "== summary.sh --window-from"

out=$("$here/summary.sh" "$run_b" --window-from "$run_a")
contains "--window-from a run directory finds its record" "$out" "load-phase  avg= 51.0%"
contains "--window-from names the record it read"          "$out" "run-record.mcp"
out=$("$here/summary.sh" "$run_b" --window-from "$run_a/run-record.mcp")
contains "--window-from a record path works too" "$out" "load-phase  avg= 51.0%"

rc=0
"$here/summary.sh" "$run_a" --window-from "$run_b" >/dev/null 2>&1 || rc=$?
eq "--window-from a directory with no stamped window fails loudly" "$rc" "2"

echo "== summary.sh rejects a window it cannot trust"

# Each of these paths exists because a mistyped invocation used to produce a
# confident report of the wrong window — or of no window, silently. Table-driven
# so the next person adding a form adds a row.
#   <label> | <expected needle in stderr> | <args...>
while IFS='|' read -r label needle args; do
  [[ -n "$label" ]] || continue
  rc=0
  err=$("$here/summary.sh" "$run_a" $args 2>&1 >/dev/null) || rc=$?
  if [[ "$rc" != 2 ]]; then
    bad "$label (rc=$rc, want 2)"
  elif [[ "$err" != *"$needle"* ]]; then
    bad "$label — rejected, but the message did not say why (wanted: $needle)"
  else
    ok "$label"
  fi
done <<TABLE
a typo'd flag is rejected, not read as an epoch|unknown option|--window-form $run_a
an unknown option is rejected|unknown option|--bogus
--window-from with no value is rejected|needs a run directory|--window-from
a single epoch is rejected, not silently dropped|needs two epochs|1788878030
a trailing extra argument is rejected|unexpected extra|1788878030 1788878055 EXTRA
a non-numeric epoch is rejected|must be integer seconds|abc 1788878055
a reversed window is rejected|must be before its end|1788878055 1788878030
an empty window is rejected|must be before its end|1788878030 1788878030
--window-from a path that is not a record is rejected|no load window stamped|--window-from $tmp/cfg-full.yaml
TABLE

# And the two valid forms must still be accepted.
for form in "$((BASE + 30)) $((BASE + 55))" "--window-from $run_a"; do
  rc=0
  "$here/summary.sh" "$run_a" $form >/dev/null 2>&1 || rc=$?
  eq "the valid form [$form] is accepted" "$rc" "0"
done

# --- perf_yaml_semp_value ----------------------------------------------------

echo "== perf_yaml_semp_value reads only the top-level semp: block"

cat >"$tmp/cfg-full.yaml" <<'YAML'
port: 9090
semp:
  max_concurrent_per_broker: 10
  request_timeout_duration: 1m
  # request_min_interval: 100ms   <- commented out, must not be read
  request_min_interval: 0s
  max_queue_wait: 45s
brokers:
  broker-01: { url: "http://${MOCK_HOST}:18081" }
  max_queue_wait: 9s
YAML

eq "reads an integer scalar"  "$(perf_yaml_semp_value "$tmp/cfg-full.yaml" max_concurrent_per_broker)" "10"
eq "reads a duration scalar"  "$(perf_yaml_semp_value "$tmp/cfg-full.yaml" request_min_interval)"      "0s"
eq "reads max_queue_wait"     "$(perf_yaml_semp_value "$tmp/cfg-full.yaml" max_queue_wait)"            "45s"
eq "a key absent from semp: reads empty" "$(perf_yaml_semp_value "$tmp/cfg-full.yaml" fair_scheduling)" ""

# The block boundary is the whole correctness argument: a same-named key under
# another top-level mapping must not be picked up, or a broker-level value
# would be recorded as the server's admission setting.
cat >"$tmp/cfg-elsewhere.yaml" <<'YAML'
semp:
  max_concurrent_per_broker: 10
brokers:
  max_queue_wait: 9s
YAML
eq "a same-named key under brokers: is not read" \
  "$(perf_yaml_semp_value "$tmp/cfg-elsewhere.yaml" max_queue_wait)" ""

cat >"$tmp/cfg-commented.yaml" <<'YAML'
semp:
  # max_queue_wait: 30s
  max_concurrent_per_broker: 10
YAML
eq "a commented-out key reads empty, not its commented value" \
  "$(perf_yaml_semp_value "$tmp/cfg-commented.yaml" max_queue_wait)" ""

# --- perf_record_admission ---------------------------------------------------

echo "== perf_record_admission never guesses a provenance"

# Only fair_scheduling is on the server's `config loaded` line today.
printf '%s\n' '{"time":"x","level":"INFO","msg":"config loaded","broker_count":50,"port":9090,"log_level":"info","fair_scheduling":true}' >"$tmp/mcp.log"

rec="$tmp/rec-admission"
: >"$rec"
perf_record_admission "$rec" "$tmp/mcp.log" "$tmp/cfg-full.yaml" 2>/dev/null
got=$(cat "$rec")

contains "fair_scheduling comes from the server's own report" "$got" "semp_fair_scheduling=true"
contains "and is labelled server-log"                          "$got" "semp_fair_scheduling_source=server-log"
contains "a setting written in the config is recorded"         "$got" "semp_max_concurrent_per_broker=10"
contains "and is labelled config-file"                         "$got" "semp_max_concurrent_per_broker_source=config-file"
contains "max_queue_wait is read from the config when present" "$got" "semp_max_queue_wait=45s"

# The story's rule: a value the harness cannot establish reads `unknown`, never
# a guess — because the defaults are a 100ms pacer, 10 slots, a 30s bound and
# fair scheduling ON, so "unset" is emphatically not "off".
rec="$tmp/rec-defaults"
: >"$rec"
cat >"$tmp/cfg-bare.yaml" <<'YAML'
port: 9090
semp:
  request_timeout_duration: 1m
YAML
perf_record_admission "$rec" "$tmp/mcp.log" "$tmp/cfg-bare.yaml" 2>/dev/null
got=$(cat "$rec")
# The real invariant is that the VALUE reads unknown for every setting the
# harness cannot establish — asserted positively, per key. The previous form
# was `lacks "…=100ms"`, which no code path could ever emit (100ms appears
# nowhere but a prose comment), so it could not fail.
for key in max_concurrent_per_broker request_min_interval max_queue_wait; do
  eq "an unestablished semp.$key reads unknown, never its server default" \
    "$(awk -F= -v k="semp_$key" '$1 == k {print $2; exit}' "$rec")" "unknown"
  eq "and semp.$key is labelled unreported-server-default" \
    "$(awk -F= -v k="semp_${key}_source" '$1 == k {print $2; exit}' "$rec")" "unreported-server-default"
done

# A server that never logged the line, versus one that logged it without the
# field. Collapsing those into one `unknown` would let a log-schema change
# silently degrade every future run with no signal.
rec="$tmp/rec-nolog"
: >"$rec"
perf_record_admission "$rec" "$tmp/does-not-exist.log" "$tmp/cfg-full.yaml" 2>/dev/null
contains "no server log at all is labelled server-log-absent" \
  "$(cat "$rec")" "semp_fair_scheduling_source=server-log-absent"

rec="$tmp/rec-schema"
: >"$rec"
printf '%s\n' '{"msg":"config loaded","broker_count":50,"port":9090}' >"$tmp/mcp-noschema.log"
perf_record_admission "$rec" "$tmp/mcp-noschema.log" "$tmp/cfg-full.yaml" 2>/dev/null
contains "a config-loaded line without the field is labelled server-log-schema-changed" \
  "$(cat "$rec")" "semp_fair_scheduling_source=server-log-schema-changed"

# The presence probe has to be scoped to the semp: block, exactly as the value
# reader is. Unscoped, a same-named key under another top-level mapping makes a
# genuine server default read `config-file-unparsed` — the wrong provenance,
# which is the class of error the whole _source scheme exists to prevent.
rec="$tmp/rec-elsewhere"
: >"$rec"
cat >"$tmp/cfg-key-under-brokers.yaml" <<'YAML'
semp:
  max_concurrent_per_broker: 10
brokers:
  broker-01: { url: "http://x:1" }
  max_queue_wait: 9s
YAML
perf_record_admission "$rec" "$tmp/mcp.log" "$tmp/cfg-key-under-brokers.yaml" 2>/dev/null
eq "a same-named key outside semp: does not make a default read config-file-unparsed" \
  "$(awk -F= '/^semp_max_queue_wait_source=/ {print $2; exit}' "$rec")" \
  "unreported-server-default"

# A `semp:` line carrying a trailing comment must not hide the whole block.
rec="$tmp/rec-semp-comment"
: >"$rec"
cat >"$tmp/cfg-semp-comment.yaml" <<'YAML'
semp:  # the pacer lives in here
  request_min_interval: 100ms
YAML
perf_record_admission "$rec" "$tmp/mcp.log" "$tmp/cfg-semp-comment.yaml" 2>/dev/null
eq "a trailing comment on the semp: line does not hide the block" \
  "$(awk -F= '/^semp_request_min_interval=/ {print $2; exit}' "$rec")" "100ms"
eq "and the value is correctly sourced to the config file" \
  "$(awk -F= '/^semp_request_min_interval_source=/ {print $2; exit}' "$rec")" "config-file"

# A shape the narrow reader cannot handle must not be reported as "nobody wrote
# it down". A wrong _source is in the same family as a wrong value.
rec="$tmp/rec-unparsed"
: >"$rec"
cat >"$tmp/cfg-flow.yaml" <<'YAML'
semp:
  max_queue_wait:
    value: 45s
YAML
perf_record_admission "$rec" "$tmp/mcp.log" "$tmp/cfg-flow.yaml" 2>/dev/null
got=$(cat "$rec")
contains "an unparseable-but-present setting reads unknown" "$got" "semp_max_queue_wait=unknown"
contains "and is labelled config-file-unparsed, not a default" "$got" "semp_max_queue_wait_source=config-file-unparsed"

# --- perf_record_fixtures ----------------------------------------------------

echo "== perf_record_fixtures reads the manifest's provenance header"

# These fields are how a run names the capture it replayed, and they had no
# test at all. The assertions are on the extracted values — the actual
# contract — rather than on the extraction mechanism: the reader strips a
# literal prefix with sub(), so there is no offset arithmetic left to pin, and
# the values below are what a consumer reads.
cat >"$tmp/fixtures.manifest" <<'MANIFEST'
# perf fixtures manifest — written by regen-golden.sh. Local only, not committed.
# run_id: 20260901T100000Z
# captured_at: 2026-09-01T10:00:00Z
# capture_commit: 0123456789abcdef0123456789abcdef01234567
# capture_dirty: false
# broker_alias: lab-broker-a
# vpn: lab_vpn_one
# rdp: lab_rdp_one
0000000000000000000000000000000000000000000000000000000000000000  mock-semp/canned/a.json
1111111111111111111111111111111111111111111111111111111111111111  fidelity/golden/b.json
MANIFEST
rec="$tmp/rec-fixtures"
: >"$rec"
perf_record_fixtures "$rec" "$tmp"
field() { awk -F= -v k="$1" '$1 == k {print $2; exit}' "$rec"; }
eq "captured_at is read exactly, with no leading or trailing character" \
  "$(field fixtures_captured_at)" "2026-09-01T10:00:00Z"
eq "capture_commit likewise"      "$(field fixtures_capture_commit)" "0123456789abcdef0123456789abcdef01234567"
eq "capture_dirty is read"        "$(field fixtures_capture_dirty)"  "false"
eq "broker_alias is read"         "$(field fixtures_broker_alias)"   "lab-broker-a"
eq "vpn is read"                  "$(field fixtures_vpn)"            "lab_vpn_one"
eq "rdp is read"                  "$(field fixtures_rdp)"            "lab_rdp_one"
eq "the fixture files are counted, not the comment lines" \
  "$(field fixtures_files)" "2"
if [[ "$(field fixtures_manifest_sha256)" == "$(sha256sum "$tmp/fixtures.manifest" | cut -d' ' -f1)" ]]; then
  ok "the manifest hash pins the whole fixture set in one field"
else
  bad "fixtures_manifest_sha256 does not match the manifest's actual hash"
fi

rec="$tmp/rec-nofixtures"
: >"$rec"
perf_record_fixtures "$rec" "$tmp/nonexistent-dir"
eq "an absent manifest reads unknown rather than empty" \
  "$(awk -F= '/^fixtures_manifest_sha256=/ {print $2; exit}' "$rec")" "unknown"

echo "== perf_stamp_load_start/_end share one clock read"

# run.sh stamps two records for one load phase. Without the optional epoch
# argument the two `date` calls could disagree by a second, and summary.sh
# reads whichever record its glob yields first — so the window a run reports
# would depend on directory order.
ra="$tmp/stamp-a" rb="$tmp/stamp-b"
: >"$ra"; : >"$rb"
# A fixed sentinel in the past, NOT `date +%s`. With the live clock as the
# sentinel, an implementation that ignored the argument and re-read the clock
# passed whenever both reads landed in the same second — so this assertion,
# which guards the fix for two records disagreeing about their window, could
# not fail. A literal makes any re-read wrong by years.
shared=1769000000
perf_stamp_load_start "$ra" "$shared"
perf_stamp_load_start "$rb" "$shared"
perf_stamp_load_end "$ra" "$((shared + 60))"
perf_stamp_load_end "$rb" "$((shared + 60))"
eq "both records carry the identical load_start_epoch" \
  "$(awk -F= '/^load_start_epoch=/ {print $2}' "$ra")" \
  "$(awk -F= '/^load_start_epoch=/ {print $2}' "$rb")"
eq "both records carry the identical load_end_epoch" \
  "$(awk -F= '/^load_end_epoch=/ {print $2}' "$ra")" \
  "$(awk -F= '/^load_end_epoch=/ {print $2}' "$rb")"
eq "the supplied epoch is used verbatim, not re-read" \
  "$(awk -F= '/^load_start_epoch=/ {print $2}' "$ra")" "$shared"
eq "load_window_source records that a runner stamped it" \
  "$(awk -F= '/^load_window_source=/ {print $2}' "$ra")" "runner-stamped"

# Omitting the argument must still work — run-loadgen.sh stamps one record and
# passes none.
rc="$tmp/stamp-c"
: >"$rc"
perf_stamp_load_start "$rc"
if [[ "$(awk -F= '/^load_start_epoch=/ {print $2}' "$rc")" =~ ^[0-9]{10}$ ]]; then
  ok "with no argument it reads the clock itself"
else
  bad "the default (no-argument) stamp path did not record an epoch"
fi

# --- perf_record_fd_peak -----------------------------------------------------

echo "== perf_record_fd_peak reads peaks by column name and skips NA"

cat >"$tmp/mem.csv" <<'CSV'
t_sec,wall_ts,rss_kb,vm_kb,threads,open_fds
0.000,x,80000,900000,14,42
1.000,x,190000,900000,58,913
2.000,x,150000,900000,31,NA
CSV
rec="$tmp/rec-peak"
: >"$rec"
perf_record_fd_peak "$rec" "$tmp/mem.csv"
eq "fd_peak is the mid-run maximum, not the last sample" \
  "$(awk -F= '/^fd_peak=/ {print $2}' "$rec")" "913"
eq "threads_peak likewise" \
  "$(awk -F= '/^threads_peak=/ {print $2}' "$rec")" "58"

# All-NA must produce no field at all: fd_peak=0 would say the server held no
# descriptors, which is never true and would be believed.
cat >"$tmp/mem-na.csv" <<'CSV'
t_sec,wall_ts,rss_kb,vm_kb,threads,open_fds
0.000,x,80000,900000,14,NA
1.000,x,80000,900000,15,NA
CSV
rec="$tmp/rec-na"
: >"$rec"
perf_record_fd_peak "$rec" "$tmp/mem-na.csv"
lacks "an all-NA fd column writes no fd_peak field at all" "$(cat "$rec")" "fd_peak"
contains "but threads_peak is still recorded"              "$(cat "$rec")" "threads_peak=15"

# A pre-open_fds mem.csv, and a column order this reader must not assume.
cat >"$tmp/mem-old.csv" <<'CSV'
t_sec,wall_ts,rss_kb,vm_kb,threads
0.000,x,80000,900000,14
CSV
rec="$tmp/rec-old"
: >"$rec"
perf_record_fd_peak "$rec" "$tmp/mem-old.csv"
lacks "a mem.csv predating open_fds writes no fd_peak" "$(cat "$rec")" "fd_peak"

cat >"$tmp/mem-reordered.csv" <<'CSV'
t_sec,wall_ts,open_fds,threads,rss_kb,vm_kb
0.000,x,777,9,80000,900000
CSV
rec="$tmp/rec-reordered"
: >"$rec"
perf_record_fd_peak "$rec" "$tmp/mem-reordered.csv"
eq "columns are found by name, not position" \
  "$(awk -F= '/^fd_peak=/ {print $2}' "$rec")" "777"

# --- perf_first_held_port ----------------------------------------------------

echo "== perf_first_held_port"

if ! command -v ss >/dev/null 2>&1; then
  skip "port checks (ss not installed)"
else
  # Two listeners, so the "first held in the caller's order" contract is
  # actually exercised rather than trivially satisfied.
  port_a=19187 port_b=19193
  python3 - "$port_a" "$port_b" <<'PY' &
import socket, sys, time
socks = []
for p in sys.argv[1:]:
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", int(p)))
    s.listen(1)
    socks.append(s)
time.sleep(6)
PY
  helper=$!
  # Give the listeners a moment; skip rather than flake if they never come up.
  for _ in $(seq 1 20); do
    perf_first_held_port "$port_a" >/dev/null && break
    sleep 0.2
  done

  if perf_first_held_port "$port_a" >/dev/null; then
    eq "reports a held port" "$(perf_first_held_port 19180 "$port_a" 19199)" "$port_a"
    eq "reports the FIRST held port in the caller's order" \
      "$(perf_first_held_port 19180 "$port_b" "$port_a")" "$port_b"
    rc=0; perf_first_held_port 19181 19182 19183 >/dev/null || rc=$?
    eq "returns non-zero when every port is free" "$rc" "1"

    # The bounded wait must fail, not hang, and must name the port.
    werr="$tmp/wait.err"
    rc=0
    perf_wait_ports_free 1 19180 "$port_a" >"$tmp/wait.out" 2>"$werr" || rc=$?
    eq "the bounded wait fails when a port stays held" "$rc" "1"
    contains "and names the port it waited on" "$(cat "$werr")" "port $port_a still has a listener"

    # PORT_WAIT_SECS=0 is the operator opting out — distinct from a missing ss.
    rc=0
    perf_wait_ports_free 0 "$port_a" >/dev/null 2>&1 || rc=$?
    eq "a zero timeout skips the check deliberately" "$rc" "0"
  else
    skip "port checks (could not bind fixture listeners)"
  fi
  kill "$helper" 2>/dev/null || true
  wait "$helper" 2>/dev/null || true

  rc=0
  perf_wait_ports_free 1 19181 >/dev/null 2>&1 || rc=$?
  eq "the wait succeeds immediately when the port is free" "$rc" "0"

  # A present-but-FAILING ss (no /proc/net, a restricted namespace) is the
  # third state: not "no listeners", but "cannot tell". Reading it as free is
  # what lets a run measure the previous point's server, which is the whole
  # reason this guard exists. Stub ss on PATH inside a subshell so the real one
  # is untouched.
  stub="$tmp/stub"
  mkdir -p "$stub"
  printf '#!/bin/sh\nexit 3\n' >"$stub/ss"
  chmod +x "$stub/ss"

  rc=0
  ( PATH="$stub:$PATH"; perf_first_held_port 19181 ) >/dev/null 2>&1 || rc=$?
  eq "a failing ss reports 'cannot tell' (rc=2), not 'free' (rc=1)" "$rc" "2"

  rc=0
  werr2="$tmp/wait-stub.err"
  ( PATH="$stub:$PATH"; perf_wait_ports_free 1 19181 ) >/dev/null 2>"$werr2" || rc=$?
  eq "and the wait then refuses to start rather than proceeding" "$rc" "1"
  # The exit code alone is not enough: with the fail-closed branch removed the
  # loop simply times out and also returns 1 — the right code for the wrong
  # reason. Only the fail-closed branch says why.
  contains "and says it refused because the port check was unusable" \
    "$(cat "$werr2")" "refusing to start without a usable port check"

  # The documented escape hatch has to work on exactly that box.
  rc=0
  ( PATH="$stub:$PATH"; perf_wait_ports_free 0 19181 ) >/dev/null 2>&1 || rc=$?
  eq "PORT_WAIT_SECS=0 still opts out when ss is unusable" "$rc" "0"
fi

# --- perf_record_kv / perf_or_unknown ---------------------------------------

echo "== the record's own field discipline"

rec="$tmp/rec-kv"
: >"$rec"
# RIG_NOTE is free text and the EC2 metadata replies come off the network; a
# newline in either would forge extra key=value lines into a file the harness's
# doctrine says a later comparison will trust.
perf_record_kv "$rec" rig_note "line one
semp_fair_scheduling=false"
eq "a value containing a newline stays one field" "$(wc -l <"$rec")" "1"
lacks "and cannot forge a second field" "$(cat "$rec")" $'\n'"semp_fair_scheduling=false"
contains "the value is flattened, not dropped" "$(cat "$rec")" "line one semp_fair_scheduling=false"

rec="$tmp/rec-long"
: >"$rec"
perf_record_kv "$rec" rig_note "$(printf 'x%.0s' $(seq 1 500))"
if (( $(awk -F= '{print length($2)}' "$rec") <= 200 )); then
  ok "an over-long value is capped"
else
  bad "an over-long value was not capped"
fi

eq "perf_or_unknown passes a real value through" "$(perf_or_unknown "i9-11900H")" "i9-11900H"
eq "an empty value becomes unknown"              "$(perf_or_unknown "")"          "unknown"
eq "a whitespace-only value becomes unknown"     "$(perf_or_unknown "   ")"       "unknown"

# --- perf_raise_nofile -------------------------------------------------------

echo "== perf_raise_nofile refuses to run where it would not take effect"

# The invariant is that it must run in the caller's own shell: a subshell's
# raised limit dies with the subshell, leaving the server on the login-shell
# limit while the record claims the raised one.
rc=0
( perf_raise_nofile 4096 ) >/dev/null 2>&1 || rc=$?
eq "called in a subshell, it fails rather than silently doing nothing" "$rc" "1"

perf_raise_nofile 4096
if [[ -n "${PERF_NOFILE_REQUESTED:-}" && -n "${PERF_NOFILE_GRANTED:-}" ]]; then
  ok "called in the caller's shell, it sets requested and granted ($PERF_NOFILE_REQUESTED/$PERF_NOFILE_GRANTED)"
else
  bad "PERF_NOFILE_REQUESTED/PERF_NOFILE_GRANTED were not set"
fi
eq "the requested value is recorded verbatim" "$PERF_NOFILE_REQUESTED" "4096"

echo
if (( skip )); then
  echo "$pass passed, $fail failed, $skip skipped"
else
  echo "$pass passed, $fail failed"
fi
[[ "$fail" -eq 0 ]]
