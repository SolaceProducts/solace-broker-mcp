#!/usr/bin/env bash
# Rolls up a run's sampler CSVs into min/avg/max lines.
# Handles both layouts:
#   single-host (run.sh):        <runs>/sampler.csv         (mcp + mock cols)
#   split-host mcp (run-mcp.sh): <runs>/sampler.csv         (mcp cols only)
#   split-host lg  (run-loadgen.sh): <runs>/mock-sampler.csv (mock cols only)
#                                <runs>/loadgen-metrics.csv (loadgen box CPU/RSS)
#
# Usage: ./summary.sh <runs-dir>
#   ./summary.sh bin/runs/20260729-131337-mcp

set -euo pipefail

runs="${1:?usage: $0 <runs-dir>}"
[[ -d "$runs" ]] || { echo "not a directory: $runs" >&2; exit 2; }

# CSV columns in sampler.csv (index):
# 1 t_sec 2 wall
# 3 mcp_cpu 4 mcp_cpu_pct_of_box 5 mcp_rss_kb 6 mcp_pss_kb 7 mcp_uss_kb
# 8 mock_cpu 9 mock_cpu_pct_of_box 10 mock_rss_kb 11 mock_pss_kb 12 mock_uss_kb
# 13 loadavg1 14 sys_mem_used_kb

# Roll one process's columns into a one-line summary.
#   csv=path  label="mcp"/"mock"  box_col=<idx>  rss_col=<idx>
#   mem_total_kb=<from info>
# We report CPU as % of the whole box (box_col) rather than the raw
# per-core column (cpu_col in sampler.csv), so cpu_col is not needed here.
roll() {
  local csv=$1 label=$2 box_col=$3 rss_col=$4 mem_total=$5
  [[ -r "$csv" ]] || return 0
  awk -F, -v bc="$box_col" -v rc="$rss_col" -v mem="$mem_total" -v L="$label" '
    NR==1 { next }
    $bc == "NA" || $bc == "ENDED" || $bc == "" { next }
    {
      n++
      # cpu as % of whole box (0-100)
      box = $bc + 0
      box_sum += box
      if (box > box_max) box_max = box
      if (box_min == "" || box < box_min) box_min = box

      # rss
      rss = $rc + 0
      rss_sum += rss
      if (rss > rss_max) rss_max = rss
      if (rss_min == "" || rss < rss_min) rss_min = rss
    }
    END {
      if (n == 0) { printf "  %-5s (no samples)\n", L; exit }
      printf "  %-5s cpu:  min=%5.1f%%   avg=%5.1f%%   max=%5.1f%%   (out of 100%% box)\n",
             L, box_min, box_sum/n, box_max
      if (mem > 0)
        printf "  %-5s mem:  min=%5.2f%%   avg=%5.2f%%   max=%5.2f%%   (out of 100%% box, %.1f GB total)\n",
               L, 100*rss_min/mem, 100*(rss_sum/n)/mem, 100*rss_max/mem, mem/1024/1024
      else
        printf "  %-5s mem:  min=%6.1f MB   avg=%6.1f MB   max=%6.1f MB\n",
               L, rss_min/1024, (rss_sum/n)/1024, rss_max/1024
    }
  ' "$csv"
}

# Try to read mem_total_kb from the sidecar so mem-as-%-of-box is meaningful.
# Falls back to 0 (roll() then skips the % column).
info_mem() {
  local info=$1
  [[ -r "$info" ]] || { echo 0; return; }
  awk -F= '/^mem_total_kb/ {print $2; exit}' "$info"
}

echo "runs dir: $runs"
echo

# Pick whichever sampler CSVs exist. Single-host has both cols in one file;
# split-host has one per box.
main_csv="$runs/sampler.csv"
mock_only_csv="$runs/mock-sampler.csv"
lg_csv="$runs/loadgen-metrics.csv"

if [[ -r "$main_csv" ]]; then
  mem=$(info_mem "$main_csv.info")
  echo "-- sampler.csv --"
  roll "$main_csv" "mcp"  4 5  "$mem"
  # mock columns exist in single-host runs; in split-host mcp runs they're all NA
  # and roll() prints "(no samples)" — that's fine.
  roll "$main_csv" "mock" 9 10 "$mem"
  echo
fi

if [[ -r "$mock_only_csv" ]]; then
  mem=$(info_mem "$mock_only_csv.info")
  echo "-- mock-sampler.csv --"
  roll "$mock_only_csv" "mock" 9 10 "$mem"
  echo
fi

# loadgen-metrics.csv layout (from loadgen-sampler.sh):
# 1 t_sec 2 wall 3 lg_cpu 4 lg_res_kb 5 loadavg1 6 sys_cpu_used_pct 7 sys_mem_used_kb 8 tcp_established 9 tcp_time_wait
# There's no lg_cpu_pct_of_box column, so compute it from CLK_TCK * nproc via .info.
if [[ -r "$lg_csv" ]]; then
  info="$lg_csv.info"
  ncores=$(awk -F= '/^cores_logical/ {print $2; exit}' "$info" 2>/dev/null || echo 1)
  mem=$(info_mem "$info")
  echo "-- loadgen-metrics.csv --"
  awk -F, -v n="$ncores" -v mem="$mem" '
    NR==1 { next }
    $3 == "ENDED" || $3 == "NA" || $3 == "" { next }
    {
      k++
      # lg CPU is reported as % of one core; divide by ncores for % of box.
      box = ($3 + 0) / n
      res = $4 + 0
      box_sum += box; if (box > box_max) box_max = box; if (box_min == "" || box < box_min) box_min = box
      res_sum += res; if (res > res_max) res_max = res; if (res_min == "" || res < res_min) res_min = res
      if ($8+0 > est_max) est_max = $8+0
    }
    END {
      if (k == 0) { print "  (no samples)"; exit }
      printf "  lg    cpu:  min=%5.1f%%   avg=%5.1f%%   max=%5.1f%%   (out of 100%% box)\n",
             box_min, box_sum/k, box_max
      if (mem > 0)
        printf "  lg    mem:  min=%5.2f%%   avg=%5.2f%%   max=%5.2f%%   (out of 100%% box, %.1f GB total)\n",
               100*res_min/mem, 100*(res_sum/k)/mem, 100*res_max/mem, mem/1024/1024
      else
        printf "  lg    mem:  min=%6.1f MB   avg=%6.1f MB   max=%6.1f MB\n",
               res_min/1024, (res_sum/k)/1024, res_max/1024
      printf "  lg    tcp:  max established=%d\n", est_max
    }
  ' "$lg_csv"
  echo
fi

# Box info footer so the percentages have context.
for info in "$main_csv.info" "$mock_only_csv.info" "$lg_csv.info"; do
  [[ -r "$info" ]] || continue
  cores=$(awk -F= '/^cores_logical/ {print $2}' "$info")
  memkb=$(awk -F= '/^mem_total_kb/ {print $2}' "$info")
  memgb=$(awk -v m="$memkb" 'BEGIN{printf "%.1f", m/1024/1024}')
  host=$(awk -F= '/^host/ {print $2}' "$info")
  mode=$(awk -F= '/^mode/ {print $2}' "$info")
  echo "box: $host  ${cores} cores  ${memgb} GB   (mode=$mode, from $(basename "$info"))"
done
