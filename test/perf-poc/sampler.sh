#!/usr/bin/env bash
# Samples MCP + mock-semp CPU + memory every N seconds via /proc and appends CSV.
# Pure /proc, no top/pidstat dependency.
#
# Memory columns:
#   rss_kb  — resident set size (what top shows, what OOM-kills on)
#   pss_kb  — proportional set size, shared pages divided by sharers (from smaps_rollup)
#   uss_kb  — unique set size, private pages only (from smaps_rollup)
# CPU is computed from utime+stime jiffie deltas in /proc/<pid>/stat, then
# normalized against interval * CLK_TCK. 100% = one full core.
#
# Also writes a <out>.info sidecar with host details so per-process CPU% and
# memory numbers can be read against the total.
#
# Usage: ./sampler.sh <out.csv> [interval_sec] [duration_sec]
#   interval defaults to 5s, duration to 90s.
#
# Mode env vars (for the split-host layout):
#   MCP_ONLY=1   sample only MCP (Box B). mock cols stay in the CSV as NA
#                so downstream parsers keep a stable column set.
#   MOCK_ONLY=1  sample only mock-semp (Box A, co-located with loadgen).
#   default      sample both (single-host smoke via run.sh).

set -euo pipefail

out="${1:?usage: $0 <out.csv> [interval_sec] [duration_sec]}"
interval="${2:-5}"
duration="${3:-90}"

MCP_ONLY="${MCP_ONLY:-0}"
MOCK_ONLY="${MOCK_ONLY:-0}"

mcp_pid=""
mock_pid=""
if [[ "$MOCK_ONLY" != "1" ]]; then
  mcp_pid="$(pgrep -n -f 'go-build.*server' || true)"
fi
if [[ "$MCP_ONLY" != "1" ]]; then
  mock_pid="$(pgrep -n -f 'mock-semp -listen-start' || true)"
fi

if [[ "$MCP_ONLY" == "1" && -z "$mcp_pid" ]]; then
  echo "MCP_ONLY=1 but MCP not found — is the server running?" >&2
  exit 1
fi
if [[ "$MOCK_ONLY" == "1" && -z "$mock_pid" ]]; then
  echo "MOCK_ONLY=1 but mock-semp not found — is it running?" >&2
  exit 1
fi
if [[ "$MCP_ONLY" != "1" && "$MOCK_ONLY" != "1" && ( -z "$mcp_pid" || -z "$mock_pid" ) ]]; then
  echo "could not find MCP ($mcp_pid) or mock-semp ($mock_pid) — is the harness running?" >&2
  echo "(for split-host runs, set MCP_ONLY=1 or MOCK_ONLY=1)" >&2
  exit 1
fi

clk_tck=$(getconf CLK_TCK)
ncores=$(nproc)

# Host info sidecar — recorded once at the start of the run so downstream
# readers know what "2290% CPU" and "800 MB RES" are relative to.
info="${out}.info"
{
  echo "host=$(hostname)"
  echo "date=$(date -Iseconds)"
  echo "kernel=$(uname -r)"
  echo "cores_logical=$ncores"
  echo "cores_physical=$(lscpu | awk -F: '/^Core\(s\) per socket/ {c=$2} /^Socket\(s\)/ {s=$2} END {gsub(/ /,"",c); gsub(/ /,"",s); print c*s}')"
  echo "cpu_model=$(awk -F: '/model name/ {gsub(/^ +/, "", $2); print $2; exit}' /proc/cpuinfo)"
  echo "mem_total_kb=$(awk '/MemTotal/ {print $2}' /proc/meminfo)"
  echo "clk_tck=$clk_tck"
  echo "mcp_pid=${mcp_pid:-NA}"
  echo "mock_pid=${mock_pid:-NA}"
  echo "mode=$( [[ "$MCP_ONLY" == "1" ]] && echo mcp-only || ( [[ "$MOCK_ONLY" == "1" ]] && echo mock-only || echo both ))"
  echo "interval_sec=$interval"
  echo "duration_sec=$duration"
} > "$info"

# Read utime+stime jiffies from /proc/<pid>/stat. Field 14 = utime, 15 = stime.
# The (comm) field can contain spaces, so anchor on the trailing ')'.
read_jiffies() {
  local pid=$1
  [[ -z "$pid" || ! -r "/proc/$pid/stat" ]] && { echo ""; return; }
  awk '{ s=$0; sub(/.*\) /, "", s); split(s, f, " "); print f[12]+f[13] }' "/proc/$pid/stat"
}

# Read PSS/RSS/USS in kB from /proc/<pid>/smaps_rollup.
read_mem() {
  local pid=$1
  [[ -z "$pid" || ! -r "/proc/$pid/smaps_rollup" ]] && { echo "NA NA NA"; return; }
  awk '
    /^Rss:/            {rss=$2}
    /^Pss:/            {pss=$2}
    /^Private_Clean:/  {uss+=$2}
    /^Private_Dirty:/  {uss+=$2}
    END {printf "%s %s %s", (rss?rss:"NA"), (pss?pss:"NA"), (uss?uss:"NA")}
  ' "/proc/$pid/smaps_rollup"
}

# Prime jiffie counters so the first sample has a valid delta.
prev_mcp_j=$(read_jiffies "$mcp_pid")
prev_mock_j=$(read_jiffies "$mock_pid")
prev_t=$(awk 'BEGIN{srand(); print systime()}')

echo "sampling MCP=${mcp_pid:-NA} mock=${mock_pid:-NA} every ${interval}s for ${duration}s → $out"
echo "host info → $info"
echo "t_sec,wall,mcp_cpu,mcp_cpu_pct_of_box,mcp_rss_kb,mcp_pss_kb,mcp_uss_kb,mock_cpu,mock_cpu_pct_of_box,mock_rss_kb,mock_pss_kb,mock_uss_kb,loadavg1,sys_mem_used_kb" > "$out"

start=$SECONDS
sleep "$interval"
while (( SECONDS - start <= duration )); do
  t=$((SECONDS - start))
  wall=$(date +%H:%M:%S)
  now=$(date +%s)
  dt=$(( now - prev_t ))
  (( dt < 1 )) && dt=1
  prev_t=$now

  loadavg=$(awk '{print $1}' /proc/loadavg)
  sys_mem=$(awk '/MemTotal/ {t=$2} /MemAvailable/ {a=$2} END {print t-a}' /proc/meminfo)

  compute() {
    local pid=$1 prev_j=$2
    if [[ -z "$pid" ]]; then
      echo "NA NA NA NA NA _"
      return
    fi
    local cur_j; cur_j=$(read_jiffies "$pid")
    if [[ -z "$cur_j" ]]; then
      # Process died mid-run. Emit NAs but keep going so partial data survives.
      echo "NA NA NA NA NA _"
      return
    fi
    local dj=$(( cur_j - prev_j ))
    (( dj < 0 )) && dj=0
    # cpu% = 100 * dj / (dt * CLK_TCK). One full core = 100.
    local cpu; cpu=$(awk -v dj="$dj" -v dt="$dt" -v hz="$clk_tck" 'BEGIN {printf "%.1f", 100.0 * dj / (dt * hz)}')
    local box; box=$(awk -v c="$cpu" -v n="$ncores" 'BEGIN {printf "%.1f", c/n}')
    local mem; mem=$(read_mem "$pid")
    echo "$cpu $box $mem $cur_j"
  }

  read mcp_cpu mcp_box mcp_rss mcp_pss mcp_uss mcp_new_j < <(compute "$mcp_pid" "$prev_mcp_j")
  read mock_cpu mock_box mock_rss mock_pss mock_uss mock_new_j < <(compute "$mock_pid" "$prev_mock_j")
  [[ "$mcp_new_j" != "_" ]] && prev_mcp_j="$mcp_new_j"
  [[ "$mock_new_j" != "_" ]] && prev_mock_j="$mock_new_j"

  echo "$t,$wall,$mcp_cpu,$mcp_box,$mcp_rss,$mcp_pss,$mcp_uss,$mock_cpu,$mock_box,$mock_rss,$mock_pss,$mock_uss,$loadavg,$sys_mem" | tee -a "$out"
  sleep "$interval"
done
