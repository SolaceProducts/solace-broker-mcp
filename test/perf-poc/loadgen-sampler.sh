#!/usr/bin/env bash
# Samples loadgen CPU/RES every N seconds via top and appends CSV.
# Runs on the load-generator box (Box A). Mirrors sampler.sh from the server
# side so both halves of a split-host run produce comparable CSVs.
#
# Usage: ./loadgen-sampler.sh <out.csv> [interval_sec] [duration_sec]
#   interval defaults to 5s, duration to 90s.

set -euo pipefail

out="${1:?usage: $0 <out.csv> [interval_sec] [duration_sec]}"
interval="${2:-5}"
duration="${3:-90}"

# Find loadgen — pattern matches ./bin/loadgen or ./loadgen/loadgen invocations.
lg_pid="$(pgrep -f 'loadgen -mcp-url' || pgrep -f 'bin/loadgen' || true)"

if [[ -z "$lg_pid" ]]; then
  echo "could not find loadgen — is it running yet? Start loadgen first, then this sampler." >&2
  exit 1
fi

info="${out}.info"
{
  echo "host=$(hostname)"
  echo "role=loadgen"
  echo "date=$(date -Iseconds)"
  echo "kernel=$(uname -r)"
  echo "cores_logical=$(nproc)"
  echo "cores_physical=$(lscpu | awk -F: '/^Core\(s\) per socket/ {c=$2} /^Socket\(s\)/ {s=$2} END {gsub(/ /,"",c); gsub(/ /,"",s); print c*s}')"
  echo "cpu_model=$(awk -F: '/model name/ {gsub(/^ +/, "", $2); print $2; exit}' /proc/cpuinfo)"
  echo "mem_total_kb=$(awk '/MemTotal/ {print $2}' /proc/meminfo)"
  echo "loadgen_pid=$lg_pid"
  echo "loadgen_cmd=$(tr '\0' ' ' < /proc/"$lg_pid"/cmdline)"
  echo "interval_sec=$interval"
  echo "duration_sec=$duration"
} > "$info"

echo "sampling loadgen=$lg_pid every ${interval}s for ${duration}s → $out"
echo "host info → $info"
echo "t_sec,wall,lg_cpu,lg_res_kb,loadavg1,sys_cpu_used_pct,sys_mem_used_kb,tcp_established,tcp_time_wait" > "$out"

start=$SECONDS
while (( SECONDS - start < duration )); do
  t=$((SECONDS - start))
  wall=$(date +%H:%M:%S)
  loadavg=$(awk '{print $1}' /proc/loadavg)

  sys_mem=$(awk '/MemTotal/ {t=$2} /MemAvailable/ {a=$2} END {print t-a}' /proc/meminfo)

  # Bail cleanly if loadgen has exited so we stop sampling zero-value rows.
  if ! kill -0 "$lg_pid" 2>/dev/null; then
    echo "$t,$wall,ENDED,ENDED,$loadavg,NA,${sys_mem},NA,NA" | tee -a "$out"
    break
  fi

  snap=$(top -b -n 1 -p "$lg_pid")
  sys_cpu=$(awk '/^%Cpu\(s\)/ {for (i=1; i<=NF; i++) if ($i ~ /id,/) {gsub(",", "", $(i-1)); printf "%.1f", 100 - $(i-1); exit}}' <<<"$snap")

  proc_line=$(tail -n +8 <<<"$snap")
  lg_cpu=$(awk -v p="$lg_pid" '$1==p {print $9}' <<<"$proc_line")
  lg_res=$(awk -v p="$lg_pid" '$1==p {print $6}' <<<"$proc_line")

  # TCP-side signals — high TIME_WAIT means loadgen isn't reusing sockets, which
  # was the fix that made the split-host run work end-to-end.
  tcp_est=$(ss -tan state established 2>/dev/null | wc -l)
  tcp_tw=$(ss -tan state time-wait 2>/dev/null | wc -l)

  echo "$t,$wall,${lg_cpu:-NA},${lg_res:-NA},$loadavg,${sys_cpu:-NA},${sys_mem},${tcp_est},${tcp_tw}" | tee -a "$out"
  sleep "$interval"
done
