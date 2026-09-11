#!/usr/bin/env bash
# Rolls up a run's sampler CSVs into min/avg/max lines.
# Handles both layouts:
#   single-host (run.sh):        <runs>/sampler.csv         (mcp + mock cols)
#   split-host mcp (run-mcp.sh): <runs>/sampler.csv         (mcp cols only)
#   split-host lg  (run-loadgen.sh): <runs>/mock-sampler.csv (mock cols only)
#                                <runs>/loadgen-metrics.csv (loadgen box CPU/RSS)
#
# Usage: ./summary.sh <runs-dir> [--window-from <dir-or-record> | <from_epoch> <to_epoch>]
#   ./summary.sh bin/runs/20260729-131337-mcp
#   ./summary.sh bin/runs/20260729-131337-mcp --window-from ../20260729-131201-loadgen-200c
#   ./summary.sh bin/runs/20260729-131337-mcp 1769000000 1769000060
#
# CPU is reported twice: over the whole run, and over the load phase alone.
#
# The whole-run figure is diluted by however long the server sat idle before
# the load started — the fidelity gate, and (split-host) the wait for the other
# box. That dilution is unstated and varies per run, which makes a raw average
# useless for comparing one run against another. CPU is the metric the
# cross-environment disagreement over the first measurement pass turns on, so
# it is the number that most needs to mean the same thing twice.
#
# The load window is not inferred from the samples. It is read from the
# load_start_epoch / load_end_epoch the runner stamped into its run record when
# it started and stopped the load. Inferring it from a CPU threshold would use
# the metric to define the window it is measured over, and would break on
# exactly the runs that matter: an idle-pacer arm and a saturated arm look
# nothing alike.
#
# The split-host MCP box cannot stamp the window — the load runs on the other
# box and the two share no channel by design — so its record says so and its
# summary reports the whole-run figure only. Point `--window-from` at the load
# box's run directory (or its record) to window it after the fact; the two run
# directories are archived together, which is the channel that does exist.
#
# Prefer `--window-from` over the bare epoch pair. The report names the record
# the window came from, so a windowed figure in an archived summary stays
# traceable — a mistyped epoch otherwise produces a plausible number rather
# than an error, and "from arguments" is the one line in the report that would
# not say where its numbers came from.

set -euo pipefail

runs="${1:?usage: $0 <runs-dir> [--window-from <dir-or-record> | <from_epoch> <to_epoch>]}"
[[ -d "$runs" ]] || { echo "not a directory: $runs" >&2; exit 2; }
shift

# Columns are resolved by NAME from each CSV's header row, never by a
# hardcoded index. "Append new columns last so every index still holds" is a
# convention a reviewer has to police; reading the header costs two lines of
# awk and makes both the pre-epoch and post-epoch layouts readable by the same
# code. It is also what lets the load-phase window find `epoch` without this
# script knowing how wide the file is.

# window_from_record <record> — echo "<start> <end> <kind>" from a run record,
# or nothing when it did not stamp a window.
#
# `stats_start_epoch` wins over `load_start_epoch` when it is there. It is only
# written for a run with a WARMUP, and it marks where the load generator's
# statistics window opens — which is not where the load opens. Reporting CPU
# over the load phase while loadgen prints percentiles over the stats window
# puts two figures for two different spans under one heading, with nothing
# saying they differ; the whole point of a warmup is that the opening stretch
# is excluded, so the CPU has to exclude it too.
window_from_record() {
  awk -F= '
    /^load_start_epoch=/  { s = $2 }
    /^stats_start_epoch=/ { ss = $2 }
    /^load_end_epoch=/    { e = $2 }
    END {
      if (e == "") exit
      if (ss != "")     print ss, e, "stats"
      else if (s != "") print s, e, "load"
    }
  ' "$1"
}

load_start="" load_end="" window_source="" window_kind=""
# The per-process lines carry this, so a figure is never labelled as covering
# the load phase when the window it was computed over deliberately excludes
# part of it. Kept the same width as "load-phase" so the columns still line up.
window_label="load-phase"

# Reject an unrecognised flag rather than letting it fall through to the bare
# epoch branch. A typo'd `--window-form` used to be read as an epoch: `date`
# and the arithmetic both errored on stderr, the provenance line vanished, and
# the report still exited 0 with whole-run-only numbers — the operator got no
# signal their window had been dropped.
if [[ "${1:-}" == -* && "${1:-}" != "--window-from" ]]; then
  echo "unknown option: $1" >&2
  echo "usage: $0 <runs-dir> [--window-from <dir-or-record> | <from_epoch> <to_epoch>]" >&2
  exit 2
fi

if [[ "${1:-}" == "--window-from" ]]; then
  if [[ -z "${2:-}" ]]; then
    echo "--window-from needs a run directory or a run-record path" >&2
    exit 2
  fi
  src="$2"
  # Accept either a run directory or a record inside one, so the operator can
  # paste whichever path they have to hand.
  if [[ -d "$src" ]]; then
    cands=("$src"/run-record.*)
  else
    cands=("$src")
  fi
  for rec in "${cands[@]}"; do
    [[ -r "$rec" ]] || continue
    read -r load_start load_end window_kind <<<"$(window_from_record "$rec")"
    if [[ -n "$load_start" && -n "$load_end" ]]; then
      window_source="$rec"
      break
    fi
  done
  if [[ -z "$window_source" ]]; then
    echo "no load window stamped in $src — it has no run record with load_start_epoch/load_end_epoch" >&2
    exit 2
  fi
  shift 2
elif [[ -n "${1:-}" ]]; then
  # A window is two epochs or nothing. A single trailing argument used to be
  # discarded in silence, which is the same failure as a typo'd flag: the
  # operator asked for a window and got a whole-run report with no complaint.
  if [[ -z "${2:-}" ]]; then
    echo "a bare window needs two epochs (<from> <to>); got one argument: $1" >&2
    echo "or use --window-from <dir-or-record>, which reads them from a run record" >&2
    exit 2
  fi
  for arg in "$1" "$2"; do
    if ! [[ "$arg" =~ ^[0-9]+$ ]]; then
      echo "window epochs must be integer seconds, got: $arg" >&2
      exit 2
    fi
  done
  if (( $1 >= $2 )); then
    echo "window start ($1) must be before its end ($2)" >&2
    exit 2
  fi
  load_start="$1"
  load_end="$2"
  window_source="command-line arguments (unverified)"
  shift 2
fi

# Anything left over was not understood. Silently ignoring it is how a
# mistyped invocation produces a confident report of the wrong window.
if (( $# > 0 )); then
  echo "unexpected extra argument(s): $*" >&2
  echo "usage: $0 <runs-dir> [--window-from <dir-or-record> | <from_epoch> <to_epoch>]" >&2
  exit 2
fi

# Nothing given: fall back to whatever a record in this directory stamped.
# Absent that too, the load-phase lines are skipped rather than guessed.
if [[ -z "$window_source" ]]; then
  for rec in "$runs"/run-record.*; do
    [[ -r "$rec" ]] || continue
    read -r load_start load_end window_kind <<<"$(window_from_record "$rec")"
    if [[ -n "$load_start" && -n "$load_end" ]]; then
      window_source="$rec"
      break
    fi
  done
fi

# An explicitly given pair of epochs is whatever the operator says it is; the
# labelling above only claims a kind when a record supplied one.
if [[ -n "$window_source" && -z "$load_start" ]]; then
  window_source=""
fi

# A window read out of a record gets the same checks a window typed on the
# command line gets. A record is a file, and a file can be truncated mid-write
# or hand-edited — and an unchecked value from one reaches `date -d` and
# arithmetic, where text produces stderr noise and a whole-run report that
# still exits 0, and a reversed pair produces a confident "no samples inside
# the stamped window". Refusing to trust the record is cheaper than either.
if [[ -n "$load_start" && -n "$load_end" ]]; then
  if ! [[ "$load_start" =~ ^[0-9]+$ && "$load_end" =~ ^[0-9]+$ ]]; then
    echo "ignoring the window in ${window_source:-the run record}: epochs are not integer seconds ($load_start, $load_end)" >&2
    load_start="" load_end="" window_source="" window_kind="" window_label="load-phase"
  elif (( load_start >= load_end )); then
    echo "ignoring the window in ${window_source:-the run record}: start ($load_start) is not before end ($load_end)" >&2
    load_start="" load_end="" window_source="" window_kind="" window_label="load-phase"
  fi
fi

# A record_version this script does not know is a warning, not a failure: the
# fields it reads are looked up by name, so an added field is harmless. Saying
# so beats a silent misread if the schema ever changes meaning.
for rec in "$runs"/run-record.*; do
  [[ -r "$rec" ]] || continue
  rv=$(awk -F= '/^record_version=/ {print $2; exit}' "$rec")
  if [[ -n "$rv" && "$rv" != "1" ]]; then
    echo "note: $(basename "$rec") is record_version=$rv; this summary.sh understands 1." >&2
  fi
done

# Roll one process's columns into a one-line summary.
#   csv=path  label="mcp"/"mock"  box_field=<header name>  rss_field=<header name>
#   mem_total_kb=<from info>  from=<epoch>  to=<epoch>
# We report CPU as % of the whole box (box_col) rather than the raw
# per-core column (cpu_col in sampler.csv), so cpu_col is not needed here.
#
# When a load window is given, a second CPU line reports the same column over
# the load phase alone, immediately under the whole-run one. The whole-run
# lines are unchanged, so a run directory from before this existed still reads
# the same way; the load-phase line is additive and clearly labelled.
#
# The window is skipped silently for a process with no samples at all (the
# mock columns of a split-host MCP run) and for a CSV predating the epoch
# column — printing "no samples in the window" twice for the same absence is
# noise, and windowing on a column that is not there is not possible.
roll() {
  local csv=$1 label=$2 box_field=$3 rss_field=$4 mem_total=$5
  local from="${6:-0}" to="${7:-0}"
  [[ -r "$csv" ]] || return 0
  awk -F, -v bf="$box_field" -v rf="$rss_field" -v mem="$mem_total" -v L="$label" \
      -v wlabel="$window_label" \
          -v from="${from:-0}" -v to="${to:-0}" '
    NR==1 {
      for (i = 1; i <= NF; i++) { gsub(/^[ \t]+|[ \t]+$/, "", $i); ix[$i] = i }
      bc = ix[bf]
      rc = ix[rf]
      ec = ix["epoch"]
      wc = ix["wall"]
      # A CSV predating the epoch column simply has no window; the whole-run
      # lines below are unchanged either way.
      windowable = (ec && from > 0 && to > 0)
      # Decided here, from the header alone, and inside this block because it
      # ends with `next` — a separate NR==1 rule below would never be reached.
      # Deciding it from the header also means a CSV with a valid header and
      # zero data rows reports the mismatch, rather than falling through to
      # "(no samples)", which reads as an idle process: a real, different state.
      if (!bc || !rc) mismatch = 1
      next
    }
    # A named column that is not in this CSV is a harness/producer mismatch,
    # reported as such. Falling through to "(no samples)" would read as an idle
    # process, which is a real and very different state.
    #
    # A flag rather than a bare `exit`: in awk, exit still runs the END block,
    # so exiting here printed the mismatch line AND a "(no samples)" line
    # under it. Caught by lib.test.sh, which is why that assertion is there.
    mismatch { exit }
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

      # Same column again, restricted to the stamped load phase.
      if (windowable && $ec+0 >= from && $ec+0 <= to) {
        w_n++
        w_sum += box
        if (box > w_max) w_max = box
        # The wall column, resolved by name like every other column here. This
        # read was $2 until round 3 caught it: on a reordered header it printed
        # whatever happened to sit in column 2 into the window-bounds field,
        # which is the only part of the load-phase line that says which samples
        # were averaged. Falls back to t_sec when the CSV carries no wall column.
        if (wc) { if (w_first == "") w_first = $wc; w_last = $wc }
        else    { if (w_first == "") w_first = $1 "s"; w_last = $1 "s" }
      }
    }
    END {
      # Decided at NR==1 from the header alone, so a CSV with a valid header
      # and zero data rows reports the mismatch too rather than falling through
      # to "(no samples)", which reads as an idle process — a real and very
      # different state.
      if (mismatch) {
        printf "  %-5s (column %s not in this CSV — header/producer mismatch)\n",
               L, (bc ? rf : bf)
        exit
      }
      if (n == 0) { printf "  %-5s (no samples)\n", L; exit }
      printf "  %-5s cpu:  min=%5.1f%%   avg=%5.1f%%   max=%5.1f%%   (out of 100%% box)\n",
             L, box_min, box_sum/n, box_max
      if (windowable) {
        if (w_n > 0)
          printf "  %-5s cpu:  %-12savg=%5.1f%%   max=%5.1f%%   (%d of %d samples, %s..%s)\n",
                 L, wlabel, w_sum/w_n, w_max, w_n, n, w_first, w_last
        else
          printf "  %-5s cpu:  %-12sno samples inside the stamped window\n", L, wlabel
      }
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
if [[ -n "$load_start" && -n "$load_end" ]]; then
  # Name which window this is. A reader comparing this figure with loadgen's
  # percentiles needs to know whether the two cover the same span.
  [[ "$window_kind" == "stats" ]] && window_label="stats-span"
  if [[ "$window_kind" == "stats" ]]; then
    # Same 12-character label width as "load phase:", so the provenance line
    # below stays aligned under either heading.
    printf 'stats span: %s..%s (%ds)\n' \
      "$(date -d "@$load_start" +%H:%M:%S)" "$(date -d "@$load_end" +%H:%M:%S)" \
      "$(( load_end - load_start ))"
    printf '            the span loadgen'"'"'s percentiles cover: the load phase\n'
    printf '            minus the warmup this run excluded from its stats.\n'
  else
    printf 'load phase: %s..%s (%ds)\n' \
      "$(date -d "@$load_start" +%H:%M:%S)" "$(date -d "@$load_end" +%H:%M:%S)" \
      "$(( load_end - load_start ))"
  fi
  # Name the record, and its role and start time when it has them, so a
  # windowed figure in an archived report stays traceable to the run that
  # measured the window.
  if [[ -r "$window_source" ]]; then
    printf '            window from %s (role=%s, started %s)\n' \
      "$window_source" \
      "$(awk -F= '/^role=/ {print $2; exit}' "$window_source")" \
      "$(awk -F= '/^started_at=/ {print $2; exit}' "$window_source")"
  else
    printf '            window from %s\n' "$window_source"
  fi
else
  echo "load phase: not stamped in this run directory — CPU below is whole-run only."
  echo "            (split-host MCP box: window it on the load box's run directory,"
  echo "             ./summary.sh $runs --window-from <box-a-run-dir>)"
fi
echo

# Pick whichever sampler CSVs exist. Single-host has both cols in one file;
# split-host has one per box.
main_csv="$runs/sampler.csv"
mock_only_csv="$runs/mock-sampler.csv"
lg_csv="$runs/loadgen-metrics.csv"

if [[ -r "$main_csv" ]]; then
  mem=$(info_mem "$main_csv.info")
  echo "-- sampler.csv --"
  roll "$main_csv" "mcp"  mcp_cpu_pct_of_box  mcp_rss_kb  "$mem" "$load_start" "$load_end"
  # mock columns exist in single-host runs; in split-host mcp runs they're all NA
  # and roll() prints "(no samples)" — that's fine.
  roll "$main_csv" "mock" mock_cpu_pct_of_box mock_rss_kb "$mem" "$load_start" "$load_end"
  echo
fi

if [[ -r "$mock_only_csv" ]]; then
  mem=$(info_mem "$mock_only_csv.info")
  echo "-- mock-sampler.csv --"
  roll "$mock_only_csv" "mock" mock_cpu_pct_of_box mock_rss_kb "$mem" "$load_start" "$load_end"
  echo
fi

# loadgen-metrics.csv, from loadgen-sampler.sh. Columns are resolved by header
# name here for the same reason they are everywhere else in this file.
#
# There is no lg_cpu_pct_of_box column, so it is computed from CLK_TCK * nproc
# via .info. There is also no epoch column, so this block reports the whole run
# only — the load box's own CPU is a property of the rig rather than of the
# product under test, so it has not been worth an extra column. If that changes,
# give loadgen-sampler.sh an epoch column and this block can window like roll()
# does.
if [[ -r "$lg_csv" ]]; then
  info="$lg_csv.info"
  ncores=$(awk -F= '/^cores_logical/ {print $2; exit}' "$info" 2>/dev/null || echo 1)
  mem=$(info_mem "$info")
  echo "-- loadgen-metrics.csv --"
  awk -F, -v n="$ncores" -v mem="$mem" '
    NR==1 {
      for (i = 1; i <= NF; i++) { gsub(/^[ \t]+|[ \t]+$/, "", $i); ix[$i] = i }
      cc = ix["lg_cpu"]; rc = ix["lg_res_kb"]; ec = ix["tcp_established"]
      next
    }
    !cc || !rc { next }
    $cc == "ENDED" || $cc == "NA" || $cc == "" { next }
    {
      k++
      # lg CPU is reported as % of one core; divide by ncores for % of box.
      box = ($cc + 0) / n
      res = $rc + 0
      box_sum += box; if (box > box_max) box_max = box; if (box_min == "" || box < box_min) box_min = box
      res_sum += res; if (res > res_max) res_max = res; if (res_min == "" || res < res_min) res_min = res
      if (ec && $ec+0 > est_max) est_max = $ec+0
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


# mem.csv is memsampler's per-process series: RSS/VmSize/threads/open
# descriptors, sampled once a second against the MCP process alone.
#
# The peaks are what the run record carries forward, so print them here too:
# a descriptor count that touched RLIMIT_NOFILE mid-run is invisible in a
# start-vs-end comparison, and it is the failure the concurrency-cap and
# >50-broker runs are looking for. "NA" rows (descriptor directory unreadable)
# are skipped rather than counted as zero.
mem_csv="$runs/mem.csv"
if [[ -r "$mem_csv" ]]; then
  echo "-- mem.csv (MCP process) --"
  awk -F, '
    NR==1 {
      for (i = 1; i <= NF; i++) { gsub(/^[ \t]+|[ \t]+$/, "", $i); ix[$i] = i }
      rc = ix["rss_kb"]; tc = ix["threads"]; fc = ix["open_fds"]
      next
    }
    rc && $rc ~ /^[0-9]+$/ {
      m++
      rss = $rc + 0
      if (rss > rss_max) rss_max = rss
      if (rss_min == "" || rss < rss_min) rss_min = rss
    }
    # Track the final row separately from the final *readable* row. An
    # unreadable /proc/<pid>/fd as the process winds down would otherwise let a
    # stale mid-run sample be reported as the end state, and a comparison would
    # trust it.
    {
      rows++
      if (tc) { th_final = $tc; if ($tc ~ /^[0-9]+$/) { if ($tc+0 > th_max) th_max = $tc+0; th_seen++ } }
      if (fc) { fd_final = $fc; if ($fc ~ /^[0-9]+$/) { if ($fc+0 > fd_max) fd_max = $fc+0; fd_seen++ } }
    }
    END {
      if (m == 0) { print "  (no samples)"; exit }
      printf "  mcp   rss:  min=%6.1f MB   max=%6.1f MB   (%d samples)\n", rss_min/1024, rss_max/1024, m
      if (th_seen) printf "  mcp   thr:  end=%s   peak=%d\n", th_final, th_max
      if (fd_seen) printf "  mcp   fds:  end=%s   peak=%d\n", fd_final, fd_max
      else if (fc)  printf "  mcp   fds:  not readable in any sample (/proc/<pid>/fd unreadable)\n"
      else          printf "  mcp   fds:  not recorded (mem.csv predates the open_fds column)\n"
    }
  ' "$mem_csv"
  # The limit the peak should be read against lives in the run record, not in
  # the CSV: it is a property of the process, not of a sample.
  for rec in "$runs"/run-record.*; do
    [[ -r "$rec" ]] || continue
    lim=$(awk -F= '/^nofile_effective_soft=/ {print $2; exit}' "$rec")
    [[ -n "$lim" ]] && printf '  mcp   fds:  limit=%s   (nofile_effective_soft, %s)\n' "$lim" "$(basename "$rec")"
  done
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
