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

# Sourced for perf_duration_secs. The warm-up conversion below was a second
# copy of it, and that copy used an exact float comparison — which
# perf_duration_secs' own comment warns rejects a legal `1.1h`, because
# 1.1 * 3600 is 3960.0000000000005. No function names collide.
# shellcheck source=lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

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
# Whether an `lg mem` line was printed, so the mem-loadgen.csv block below
# only cross-references a line that is actually in this report: a run
# directory can hold one series without the other.
lg_mem_reported=0
if [[ -r "$lg_csv" ]]; then
  lg_mem_reported=1
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
      for (i = 1; i <= NF; i++) { gsub(/^[ \t\r]+|[ \t\r]+$/, "", $i); ix[$i] = i }
      rc = ix["rss_kb"]; tc = ix["threads"]; fc = ix["open_fds"]
      hdr_nf = NF
      next
    }
    # Same guard as the loadgen series below, for the same reason: the runner
    # SIGKILLs this sampler on every terminated run, and a kill between two
    # writes leaves a final row truncated mid-field. "1799,14:22:31,17" parses
    # as a valid 17 KB RSS and reported `min= 0.0 MB` for a server that never
    # went below 170 MB, while leaving the thread and descriptor ends blank.
    # A genuinely empty trailing record is not a truncated write, and saying
    # "a sampler killed mid-write" about one sends the reader hunting a kill
    # that never happened.
    NF == 0 || $0 ~ /^[ \t\r]*$/ { next }
    NF != hdr_nf { short++; next }
    # The header rule strips \r; the row fields were not. A CRLF file with
    # rss_kb in the LAST column therefore gave $rc a trailing carriage
    # return, failed the digit test on every row, and reported (no samples)
    # for good data — the Windows round-trip this reader claims to handle.
    { for (i = 1; i <= NF; i++) gsub(/\r$/, "", $i) }
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
      if (short) printf "  mcp   rss:  %d row(s) ignored: fewer fields than the header (a sampler killed mid-write)\n", short
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

# mem-loadgen.csv is the same memsampler series taken against the load
# generator's own process. It exists because the generator was the one process
# in the rig nobody measured: a run holding thousands of sessions open for
# hours was only ever watched from outside the repo, and that is how its
# O(rate x duration) sample retention was found. Reported here so a regression
# shows up in the ordinary summary rather than in a one-off script.
#
# Drift, not just peak: a generator that climbs steadily and one that allocates
# its ceiling in the first minute have the same peak and completely different
# stories, and only the first is a leak.
# How much of the generator's opening stretch the drift baseline skips.
# memsampler starts as loadgen launches, so the first samples are taken during
# dialAll — a generator that allocates its sessions and then stays flat would
# otherwise report the ramp as a leak. Warm-up counts as ramp too: with WARMUP
# set, the stats window does not even open until it has elapsed.
#
# LG_DRIFT_SETTLE_SECS overrides the 30s margin. It is a margin and not a
# measurement — the honest fix is an epoch column on memsampler so this can
# window on stats_start_epoch the way the CPU roll-up does, which is a change
# to the CSV schema every archived run is read with.
lg_settle_secs=${LG_DRIFT_SETTLE_SECS:-30}
# Validated, because it is copied into an awk numeric comparison. `banana`
# string-compares against t_sec, `-1` collided with the sentinel below, and
# `1.5` silently moved the baseline — each producing the fabricated drift this
# block exists to refuse, while the report claimed a clean skip.
if [[ ! "$lg_settle_secs" =~ ^[0-9]+$ ]]; then
  echo "LG_DRIFT_SETTLE_SECS='$LG_DRIFT_SETTLE_SECS' is not a whole number of seconds" >&2
  lg_settle_secs=-2
fi
for rec in "$runs"/run-record.*; do
  [[ -r "$rec" ]] || continue
  lg_warm=$(awk -F= '/^stats_warmup=/ {print $2; exit}' "$rec")
  # `continue`, not `break`: the sibling reader for mcp_start_epoch scans on
  # when a record lacks the field, and stopping at the first record here leaned
  # on run-record.loadgen sorting first — true of every layout the runners
  # write today, and written down nowhere.
  [[ -z "$lg_warm" ]] && continue
  [[ "$lg_warm" == none ]] && break
  # Converted in awk, not by stripping a suffix into `$(( ))`. The runners
  # accept any duration perf_duration_secs accepts, and that includes
  # fractional notation landing on a whole second — `1.5m` is documented as
  # valid. Bash cannot evaluate `1.5 * 60`: the arithmetic errored mid-report,
  # the settle silently stayed at its default, and the baseline was taken
  # inside the warm-up ramp, which is the wrong-sign leak verdict this skip
  # exists to prevent.
  lg_warm_secs=$(perf_duration_secs "$lg_warm" 2>/dev/null) || lg_warm_secs=""
  if [[ -n "$lg_warm_secs" && "$lg_settle_secs" != -2 ]]; then
    lg_settle_secs=$(( lg_settle_secs + lg_warm_secs ))
  else
    # Refused, not defaulted. Falling back to the bare 30s would measure the
    # drift from inside a warm-up nobody could size.
    echo "cannot resolve stats_warmup='$lg_warm' from $(basename "$rec"); the generator drift is not reported" >&2
    lg_settle_secs=-1
  fi
  break
done

lg_mem_csv="$runs/mem-loadgen.csv"
if [[ -r "$lg_mem_csv" ]]; then
  echo "-- mem-loadgen.csv (loadgen process) --"
  awk -F, -v have_lg_mem="$lg_mem_reported" -v settle="$lg_settle_secs" '
    # \r in the class as well as space and tab: a CSV round-tripped through a
    # Windows editor during triage otherwise leaves the last header name as
    # "open_fds\r", ix["rss_kb"] still resolves, and the section silently
    # reports (no samples) if rss_kb happened to be last.
    NR==1 {
      for (i = 1; i <= NF; i++) { gsub(/^[ \t\r]+|[ \t\r]+$/, "", $i); ix[$i] = i }
      rc = ix["rss_kb"]; tc = ix["t_sec"]
      hdr_nf = NF
      next
    }
    # A row must have the width the header declared. memsampler is SIGKILLed by
    # the runner cleanup on every terminated run, and a kill between two writes
    # leaves a final row truncated mid-field — "3,10:00:03,80" parses as a
    # perfectly good 80 KB RSS. Counting it turned a generator that grew
    # 586 -> 684 MB into "drift=-585.9 MB": the leak verdict with its sign
    # inverted, on a number a campaign is compared on.
    # A genuinely empty trailing record is not a truncated write, and saying
    # "a sampler killed mid-write" about one sends the reader hunting a kill
    # that never happened.
    NF == 0 || $0 ~ /^[ \t\r]*$/ { next }
    NF != hdr_nf { short++; next }
    # The header rule strips \r; the row fields were not. A CRLF file with
    # rss_kb in the LAST column therefore gave $rc a trailing carriage
    # return, failed the digit test on every row, and reported (no samples)
    # for good data — the Windows round-trip this reader claims to handle.
    { for (i = 1; i <= NF; i++) gsub(/\r$/, "", $i) }
    rc && $rc ~ /^[0-9]+$/ {
      m++
      rss = $rc + 0
      if (rss > rss_max) rss_max = rss
      if (rss_min == "" || rss < rss_min) rss_min = rss
      # Ends as a median of up to five samples each, not one sample each. One
      # sample is one bad row away from the wrong answer, and the start of a
      # run is exactly where a generator is still dialling sessions.
      # Baseline taken after the ramp, not from the first rows. memsampler
      # starts as loadgen launches, so rows 1..5 land inside dialAll: a
      # generator that dialled 2,000 sessions in 8s and then sat perfectly
      # flat at 600 MB reported drift=+404.7 MB — a leak verdict on a bounded
      # generator, which is the one number this series exists to produce.
      # t_sec is seconds since the sampler started, within a second of the
      # generator starting.
      # Without a usable t_sec there is no way to locate the ramp, and awk
      # coerces a missing or non-numeric column to 0 — accepting every row
      # while the line below still claimed the opening seconds were skipped.
      if (!tc || $tc !~ /^[0-9]+(\.[0-9]+)?$/) { no_tsec = 1; next }
      if ($tc + 0 < settle) { skipped++; next }
      if (fn < 5) { first[++fn] = rss; if (first_t == "") first_t = $tc + 0 }
      last[(++ln - 1) % 5 + 1] = rss
      last_t = $tc + 0
    }
    function med(a, n,   i, j, t, b) {
      for (i = 1; i <= n; i++) b[i] = a[i]
      for (i = 2; i <= n; i++) { t = b[i]; for (j = i - 1; j >= 1 && b[j] > t; j--) b[j+1] = b[j]; b[j+1] = t }
      if (n % 2) return b[(n + 1) / 2]
      return (b[n/2] + b[n/2 + 1]) / 2
    }
    END {
      if (m == 0) { print "  (no samples)"; exit }
      printf "  lg    rss:  min=%6.1f MB   max=%6.1f MB   (%d samples)\n", rss_min/1024, rss_max/1024, m
      ln_eff = (ln > 5 ? 5 : ln)
      # The two ends must not be the same samples. Below eleven rows the
      # five-sample windows overlap, and at five or fewer they are identical —
      # which reported drift=+0.0 MB for a generator climbing 97.7 -> 293.0 MB.
      # A refused number beats a wrong one: a terminated run, a dial failure or
      # a short smoke DURATION all land here.
      if (no_tsec) {
        print "  lg    rss:  drift=n/a   (no usable t_sec column, so the ramp-up cannot be located)"
      } else if (settle == -2) {
        print "  lg    rss:  drift=n/a   (LG_DRIFT_SETTLE_SECS is not a whole number of"
        print "              seconds, so the ramp-up to skip is unknown — see stderr)"
      } else if (settle == -1) {
        print "  lg    rss:  drift=n/a   (stats_warmup in the run record could not be resolved,"
        print "              so the ramp-up to skip is unknown — see the warning on stderr)"
      } else if (ln < fn + 5) {
        printf "  lg    rss:  drift=n/a   (only %d samples after the %ds ramp-up skip; the end windows would overlap)\n", ln, settle
      } else {
        fs = med(first, fn); ls = med(last, ln_eff)
        printf "  lg    rss:  drift=%+.1f MB   (t=%ds median=%6.1f MB over %d -> t=%ds median=%6.1f MB over %d; first %ds skipped as ramp-up)\n", \
          (ls - fs)/1024, first_t, fs/1024, fn, last_t, ls/1024, ln_eff, settle
      }
      if (short) printf "  lg    rss:  %d row(s) ignored: fewer fields than the header (a sampler killed mid-write)\n", short
      if (have_lg_mem) print "  lg    rss:  the same quantity as the lg mem line above, sampled at 1s not 5s"
    }
  ' "$lg_mem_csv"
  echo
fi

# gctrace: the live heap, which is the only series that answers "does the
# server leak". RSS cannot — it does not separate a live heap that is growing
# from an allocator holding freed spans, and that ambiguity is what SOL-154158
# had to resolve with a script outside this repo. Under GODEBUG=gctrace=1 the
# runtime prints H_T->H_a->H_m per cycle and the third figure is the heap
# marked live.
#
# Silent when the log holds no gctrace lines: GODEBUG is opt-in, and an empty
# section or a zero would read like a measurement of a server that never
# reported one.
gc_log="$runs/mcp.log"
# Unanchored, matching the parser. Anchored at ^, a capture whose gctrace
# lines were all prefixed by an interleaved slog record skipped the whole
# section and reported no loss at all.
if [[ -r "$gc_log" ]] && grep -qE 'gc [0-9]+ @[0-9.]+s' "$gc_log"; then
  # gctrace stamps @<seconds since program start>, so placing a cycle on the
  # wall clock needs the moment the process began. The runners stamp it; a run
  # directory from before they did cannot be windowed.
  #
  # Validated as an integer for the same reason the window epochs are: a record
  # can be truncated mid-write or hand-edited, and a non-integer reaches awk as
  # 0, which excludes every cycle and reads as "the gctrace format changed".
  gc_anchor=""
  gc_anchor_src=""
  for rec in "$runs"/run-record.*; do
    [[ -r "$rec" ]] || continue
    gc_anchor=$(awk -F= '/^mcp_start_epoch=/ {print $2; exit}' "$rec")
    if [[ -n "$gc_anchor" ]]; then gc_anchor_src=$(basename "$rec"); break; fi
  done
  if [[ -n "$gc_anchor" ]] && ! [[ "$gc_anchor" =~ ^[0-9]+$ ]]; then
    echo "ignoring mcp_start_epoch in ${gc_anchor_src:-the run record}: not integer seconds ($gc_anchor)" >&2
    gc_anchor="" gc_anchor_src=""
  fi

  gc_windowed=0
  if [[ -n "$gc_anchor" && -n "${load_start:-}" && -n "${load_end:-}" ]]; then
    gc_windowed=1
  fi

  # The parse, once, shared by both passes below. Two passes rather than one,
  # because the drift windows are sized from the run's own span and the span is
  # not known until the file ends. Retaining a position per cycle to fill them
  # afterwards is what the histogram below exists to avoid: at the 1.6M cycles
  # the SOL-154158 soak logged, per-cycle arrays cost 430 MB of RSS — the same
  # O(rate x duration) retention shape this ticket exists to remove, in the
  # tool that reports on it. Two passes over the file cost a second re-read and
  # keep memory at O(distinct heap sizes).
  gc_parse='
    function parse(  i, n, tri) {
      # Anchored here rather than in the rule pattern. The rule matches a gc
      # stamp anywhere on the line, so a slog record that landed at the START
      # of one — both streams are the server stderr, captured into one
      # file — reaches
      # this function instead of failing the pattern and vanishing from both
      # counters. It is a lost cycle either way, and the point is to say so.
      if ($0 !~ /^gc [0-9]+ @/) { unparsable++; return 0 }
      t = $3; sub(/^@/, "", t); sub(/s$/, "", t)
      live = ""
      # Field-scan rather than match(): the triple is one whitespace-delimited
      # token, and this has to parse under mawk as well as gawk.
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^[0-9]+->[0-9]+->[0-9]+$/) { n = split($i, tri, "->"); live = tri[3] + 0; break }
      }
      # Not a cycle we can read. Counted, because gctrace from the Go runtime and
      # slog both write to the server stderr (cmd/server/main.go builds its
      # JSON handler on os.Stderr) and the runner captures that one stream
      # into mcp.log — so a gctrace line, which the runtime emits as several
      # small writes, can be split by a JSON record landing mid-line. Dropping those silently under-reports
      # `cycles=` and skews the median toward whichever cycles survived, with
      # no signal in the report that anything was lost.
      if (live == "") { unparsable++; return 0 }
      parsed++
      # A restart is a step BACKWARDS in the program clock, and it has to be
      # seen here: inferring it from last-minus-first only catches the case
      # where the second lifetime ends below where the first one began. A
      # 3600-cycle run followed by a 600-cycle restart reported drift=-124 MB
      # for a heap that ROSE 52 -> 300 MB, with a last window of 3120 cycles.
      if (prev_t != "" && t + 0 < prev_t) restarts++
      prev_t = t + 0
      if (windowed) {
        pos = anchor + t
        if (pos < ws || pos > we) return 0
      } else {
        # `+ 0` is load-bearing. t is the string sub() left behind on $3, and
        # the end-window tests below compare it against fp + w — a string
        # against a number is a LEXICOGRAPHIC comparison in awk, which picked
        # nonsense windows (546 and 110 cycles out of 600) while the numeric
        # overlap guard sat there and never fired.
        pos = t + 0
      }
      return 1
    }
  '

  read -r gc_parsed gc_cycles gc_first gc_last gc_unparsable gc_restarts <<<"$(
    awk -v anchor="${gc_anchor:-0}" -v ws="${load_start:-0}" -v we="${load_end:-0}" \
        -v windowed="$gc_windowed" "$gc_parse"'
      /gc [0-9]+ @[0-9.]+s/ {
        if (!parse()) next
        cycles++
        if (first_pos == "") first_pos = pos
        last_pos = pos
      }
      END { printf "%d %d %.3f %.3f %d %d\n", parsed, cycles, first_pos + 0, last_pos + 0, unparsable + 0, restarts + 0 }
    ' "$gc_log")"

  echo "-- gctrace (server live heap) --"
  if (( gc_cycles == 0 )); then
    # Parsed-but-excluded and not-parsed-at-all are different failures and lead
    # the reader to different places: a clock skew between the two boxes of a
    # split-host run, versus a gctrace format change.
    if (( gc_parsed > 0 )); then
      printf '  mcp   gc:   %d cycles parsed, none inside the window\n' "$gc_parsed"
      printf '              (anchor=%s, window=%s..%s — check the two boxes agree on the clock)\n' \
        "${gc_anchor:-none}" "${load_start:-none}" "${load_end:-none}"
    else
      echo "  mcp   gc:   no parsable gctrace cycles — the line format may have changed"
    fi
  else
    awk -v anchor="${gc_anchor:-0}" -v ws="${load_start:-0}" -v we="${load_end:-0}" \
        -v windowed="$gc_windowed" -v fp="$gc_first" -v lp="$gc_last" \
        -v restarts="$gc_restarts" "$gc_parse"'
      BEGIN {
        span = lp - fp
        # A non-positive span means @t went backwards: the log holds more than
        # one program lifetime (a hand-restarted server, or two run
        # directories concatenated during triage). Sized from it, the end
        # windows select opposite ends and the drift comes out with the wrong
        # sign — a 52 -> 300 MB climb reported as -248 MB. There is no run to
        # measure a drift over, so none is reported.
        # Negative and zero are different faults. Negative means @t went
        # backwards; zero means every cycle carries one timestamp, which a
        # capture of a few cycles inside one second does.
        # restarts is authoritative; span can still be positive across one.
        if (restarts > 0) { no_span = "lifetimes" }
        else if (span < 0) { no_span = "lifetimes" }
        else if (span == 0) { no_span = "instant" }
        # An hour, or a tenth of the run when the run is shorter than ten
        # hours. Whichever is smaller: a window longer than a tenth of the run
        # stops being an end and starts being the middle.
        w = 3600
        if (span / 10 < w) w = span / 10
        if (w <= 0) w = span
      }
      /gc [0-9]+ @[0-9.]+s/ {
        if (!parse()) next
        cycles++
        cnt[live]++
        if (lo == "" || live < lo) lo = live
        if (hi == "" || live > hi) hi = live
        if (pos <= fp + w) { fcnt[live]++; ftot++ }
        if (pos >= lp - w) { lcnt[live]++; ltot++ }
      }
      # median of a value->count histogram. A histogram, not a sort: the values
      # are a narrow band of integer MB, so counting is O(range) where sorting
      # would be O(n log n) over an array mawk has no asort for.
      function hmedian(h, l, u, total,   acc, v, want, m1, m2) {
        if (total == 0) return ""
        want = int((total + 1) / 2)
        acc = 0
        for (v = l; v <= u; v++) {
          if (!(v in h)) continue
          acc += h[v]
          if (m1 == "" && acc >= want) m1 = v
          if (total % 2 == 0) { if (acc >= want + 1) { m2 = v; break } }
          else if (m1 != "") { m2 = m1; break }
        }
        if (m2 == "") m2 = m1
        return (m1 + m2) / 2
      }
      END {
        med  = hmedian(cnt,  lo, hi, cycles)
        fmed = hmedian(fcnt, lo, hi, ftot)
        lmed = hmedian(lcnt, lo, hi, ltot)
        printf "  mcp   gc:   cycles=%d   live heap: median=%.0f MB   min=%d MB   max=%d MB\n", cycles, med, lo, hi
        # The same floor the loadgen series applies, for the same reason and
        # with the same preference: a refused number beats a wrong one.
        # Sized by time alone, a 90s smoke run puts one cycle in each end
        # window and reported a 448 MB "leak" off a single transient cycle on
        # a heap that never moved. Five per end, and the two ends must not be
        # the same cycles.
        if (no_span == "lifetimes") {
          print "  mcp   gc:   drift=n/a   (the gctrace clock goes backwards in this capture:"
          print "              it holds more than one server lifetime, so there is no single"
          print "              run to measure a drift over)"
        } else if (no_span == "instant") {
          printf "  mcp   gc:   drift=n/a   (all %d cycles carry the same timestamp; no span to measure over)\n", cycles
        } else if (ftot < 5 || ltot < 5 || fp + w >= lp - w) {
          printf "  mcp   gc:   drift=n/a   (only %d and %d cycles in the end windows; too few, or they overlap)\n", ftot, ltot
        } else if (fmed != "" && lmed != "") {
          printf "  mcp   gc:   drift=%+.0f MB   (first %ds median=%.0f MB over %d cycles -> last %ds median=%.0f MB over %d cycles)\n", \
            lmed - fmed, w, fmed, ftot, w, lmed, ltot
        }
      }
    ' "$gc_log"
  fi

  if (( gc_unparsable > 0 )); then
    printf '  mcp   gc:   %d gctrace line(s) unparsable and not counted (a JSON log\n' "$gc_unparsable"
    echo   "              record can land mid-line: gctrace and slog share the server's stderr)"
  fi

  if (( gc_windowed )); then
    # Name where the window came from, not just that there was one. A window
    # typed on the command line is marked unverified everywhere else in this
    # report (see the header line), and the gc line is the one that gets pasted
    # into a soak write-up — a mistyped epoch otherwise yields a plausible
    # live-heap median with no caveat attached.
    printf '  mcp   gc:   windowed: %s (window: %s; anchor: mcp_start_epoch in %s)\n' \
      "$window_label" \
      "$(if [[ -f "$window_source" ]]; then basename "$window_source"; else echo "${window_source:-run record}"; fi)" \
      "$gc_anchor_src"
  elif [[ -z "$gc_anchor" ]]; then
    echo "  mcp   gc:   NOT windowed — no usable mcp_start_epoch in the run record, so a"
    echo "              program-relative @Xs cannot be placed on the wall clock. The"
    echo "              figures above cover the whole capture, including any idle"
    echo "              stretch before and after the load: read min/max/drift with care."
  else
    echo "  mcp   gc:   NOT windowed — this run directory stamped no load window (the"
    echo "              split-host MCP box cannot: the load runs on the other box). The"
    echo "              figures above cover the whole capture, including the idle stretch"
    echo "              before the load, so min/max and the drift sign can both mislead."
    echo "              Re-run with --window-from <the load box's run directory>."
  fi
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
