# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# lib.sh — helpers shared by run.sh, run-mcp.sh and run-loadgen.sh.
#
# Sourced, never executed. Three concerns live here, all of them about making
# one measurement run comparable with another:
#
#   1. the run record  — what rig, what code, what fixtures, what settings
#   2. the load window — the two timestamps that let summary.sh report CPU over
#                        the load phase instead of over the whole hold
#   3. preconditions   — a free port and a raised descriptor limit
#
# Everything here is deliberately shell-and-/proc only: no jq, no python, no
# curl beyond the one optional EC2 metadata probe. A measurement harness that
# needs a toolchain installed to describe its own run is a harness that stops
# describing it on the first host that lacks the toolchain.
#
# Every function is prefixed perf_ and every global PERF_, so a sourcing script
# can tell at a glance what came from here.
#
# Deliberately NOT here: kill_tree, wait_for_http and wait_for_tcp, which each
# runner still carries its own copy of. They are process-lifecycle code whose
# copies have already diverged (run-mcp.sh has no wait_for_tcp), they are wired
# into three different trap/cleanup orderings, and nothing in this suite can
# test them without a live broker — so consolidating them is a change with real
# risk and no coverage, and it does not belong in the same diff as the run
# record. It is worth doing; it is worth doing on its own. See test/e2e-common/lib.sh
# for where they should end up.

# ---------------------------------------------------------------------------
# Ports
# ---------------------------------------------------------------------------

# perf_first_held_port <port>... — print the first of the named ports that has
# a listener, and return 0; return 1 when all of them are free.
#
# "Free" deliberately means "no listener", not "no socket". A socket in
# TIME_WAIT does not stop a bind with SO_REUSEADDR, so waiting for every socket
# on the port to disappear would wait out a 60s kernel timer for nothing — or,
# worse, time out and fail a run that could have started.
#
# Matches on the State column rather than a trailing-space grep so an IPv6
# listener ([::]:9090) counts too, and so the header row cannot match.
#
# One `ss` call for the whole set, not one per port: a 200-broker run passes
# 202 ports, and a poll loop that shelled out per port would spend more time in
# `ss` than the port takes to clear.
perf_first_held_port() {
  local ports="$*" listing
  # Capture ss separately rather than piping it, so a present-but-failing ss
  # (no /proc/net, restricted namespace) is distinguishable from "no listeners".
  # Piping it made both cases produce empty input, which the caller read as
  # "every port free" — the exact indistinguishability this guard is for.
  if ! listing=$(ss -ltn 2>/dev/null); then
    echo "ss failed — cannot determine which ports are listening" >&2
    return 2
  fi
  printf '%s\n' "$listing" | awk -v want="$ports" '
    BEGIN { n = split(want, w, " "); for (i = 1; i <= n; i++) order[i] = w[i] }
    $1 != "LISTEN" { next }
    {
      # $4 is Local Address:Port — take the text after the last colon, which
      # is the port for both 0.0.0.0:9090 and [::]:9090.
      c = split($4, a, ":")
      listening[a[c]] = 1
    }
    END {
      for (i = 1; i <= n; i++) {
        if (order[i] in listening) { print order[i]; exit 0 }
      }
      exit 1
    }
  '
}

# perf_wait_ports_free <timeout_s> <port>... — poll until no listener holds any
# of the named ports, or fail naming the one that stayed held.
#
# This exists because a back-to-back sweep point was silently lost: the
# previous point's MCP server still held :9090 when the next one started. The
# old behaviour was to abort immediately, which turns a two-second wait into a
# missing data point in the middle of a sweep. Starting anyway is worse still —
# the run would measure the *previous* server.
#
# Bounded on purpose. A port held past the timeout is a stuck process, not a
# slow one, and reporting which port was held is what makes that diagnosable.
perf_wait_ports_free() {
  local timeout_s=$1; shift

  # Validate the timeout before using it in arithmetic. It comes from
  # PORT_WAIT_SECS, so a plausible fat-finger ("60s") otherwise produced two
  # raw `value too great for base` errors, left $deadline unset, and degraded
  # the guard to a single poll — bash internals instead of the deliberate
  # diagnostic this function exists to give.
  if ! [[ "$timeout_s" =~ ^[0-9]+$ ]]; then
    echo "PORT_WAIT_SECS must be a non-negative integer number of seconds, got: $timeout_s" >&2
    return 1
  fi

  # The opt-out comes FIRST, before the ss probe. An explicit zero is the
  # operator saying "I know what is on this box, do not check", and it has to
  # work on a box that has no ss — which is precisely the box the branch below
  # tells them to use it on. Checking ss first made that instruction a dead
  # end.
  if (( timeout_s == 0 )); then
    echo "   PORT_WAIT_SECS=0 — skipping the port-free check"
    return 0
  fi

  # Fail closed if we cannot look. An `ss` that is missing or broken makes
  # perf_first_held_port print nothing, which is indistinguishable from "every
  # port is free" — so the guard that exists because a sweep point was lost to
  # a held :9090 would silently stop guarding, and the run would measure the
  # previous server. "Cannot tell" is not "safe to start".
  if ! command -v ss >/dev/null 2>&1; then
    echo "ss (iproute2) not found — cannot verify the ports are free, refusing to start." >&2
    echo "   Install iproute2, or set PORT_WAIT_SECS=0 deliberately to skip the check." >&2
    return 1
  fi

  local deadline=$((SECONDS + timeout_s))
  local held rc announced=0
  while :; do
    rc=0
    held=$(perf_first_held_port "$@") || rc=$?
    # rc 1 = every port free. rc 2 = ss could not tell us, which is
    # fail-closed: starting on an unverifiable box is what lost a sweep point.
    (( rc == 1 )) && return 0
    if (( rc > 1 )); then
      echo "   refusing to start without a usable port check (set PORT_WAIT_SECS=0 to override)" >&2
      return 1
    fi
    if (( SECONDS >= deadline )); then
      echo "port $held still has a listener after ${timeout_s}s — refusing to start:" >&2
      ss -ltnp 2>/dev/null | awk -v p="$held" '$1=="LISTEN" { c=split($4,a,":"); if (a[c]==p) print "   " $0 }' >&2
      echo "   a previous run is still holding it. Kill it, or raise PORT_WAIT_SECS." >&2
      return 1
    fi
    if (( ! announced )); then
      echo "   waiting up to ${timeout_s}s for port $held (and any other still held) to be released..."
      announced=1
    fi
    sleep 0.5
  done
}

# ---------------------------------------------------------------------------
# Descriptor limit
# ---------------------------------------------------------------------------

# perf_raise_nofile [want] — raise RLIMIT_NOFILE for this shell and everything
# it spawns, and record what was asked for versus what was granted.
#
# Sets PERF_NOFILE_REQUESTED and PERF_NOFILE_GRANTED. MUST be called in the
# runner's own shell, not in a $(...) — a subshell's raised limit dies with the
# subshell and the server would inherit the old one.
#
# A flat generous request rather than an arithmetic one derived from the broker
# count and the concurrency cap: the arithmetic is fragile (two idle pools per
# broker, each up to the cap, plus one inbound socket per load-generator
# session) and the descriptors are free. Falls back to the hard limit when that
# is lower, which is the common unprivileged case.
perf_raise_nofile() {
  # Enforce the invariant the comment above states, rather than trusting a
  # reviewer to police it. A subshell's raised limit dies with the subshell, so
  # a mis-call leaves the server running under the operator's login-shell limit
  # while the record claims the raised one — a silently wrong record, which is
  # the one failure mode this whole file exists to prevent.
  if [[ "$BASHPID" != "$$" ]]; then
    echo "perf_raise_nofile called in a subshell — the raised limit would not survive" >&2
    return 1
  fi
  local want=${1:-1048576}
  # NOFILE is operator input and reaches an arithmetic comparison below, where
  # bash treats a non-numeric word as a variable name — so `NOFILE=abc` aborted
  # the whole runner under `set -u` with "abc: unbound variable", from inside a
  # library, several steps before anything measured. Validate it here where the
  # message can name the variable.
  if ! [[ "$want" =~ ^[0-9]+$ ]] || (( want < 1 )); then
    echo "NOFILE must be a positive integer number of descriptors, got: $want" >&2
    return 1
  fi
  local hard target
  hard=$(ulimit -Hn)
  target=$want
  if [[ "$hard" != "unlimited" ]] && (( hard < want )); then
    target=$hard
  fi
  ulimit -n "$target" 2>/dev/null || true
  PERF_NOFILE_REQUESTED=$want
  PERF_NOFILE_GRANTED=$(ulimit -n)
}

# ---------------------------------------------------------------------------
# Run record
# ---------------------------------------------------------------------------
#
# Format is one key=value per line with '#' comments, the shape sampler.sh's
# .info sidecar already uses and summary.sh already parses with `awk -F=`.
# JSON would only pay for itself if something outside the harness consumed the
# record, and nothing does.
#
# The governing rule for every field below: an absent field is honest, a
# guessed one is a trap. A later comparison will trust whatever is written
# here, so anything we cannot establish is either omitted or written as the
# literal `unknown` — never inferred.

# perf_record_kv <record> <key> <value> — append one field.
#
# The value is flattened to one line and capped. Two of the inputs here are not
# fully trusted: RIG_NOTE is free text from the operator, and the EC2 metadata
# responses come off the network — on a box where 169.254.169.254 is a
# container network or a hostile LAN rather than AWS, the reply is whatever
# that endpoint chose to send. A newline in either would inject arbitrary extra
# `key=value` lines into a file this harness's own doctrine says "a later
# comparison will trust", so a forged `semp_fair_scheduling=false` is worth one
# line of defence at the single choke point every field passes through.
perf_record_kv() {
  local value=${3-}
  value=${value//$'\n'/ }
  value=${value//$'\r'/ }
  value=${value//$'\t'/ }
  # '=' is the field separator. The record's documented format is one
  # key=value per line, read with `awk -F=`, so a value containing '=' reads
  # back truncated at the first one — RIG_NOTE="instance=m5.large" became
  # "instance". Substituted rather than rejected, because losing the note
  # entirely is worse than losing one character, and warned rather than
  # silently altered, because a provenance file that quietly rewrites what it
  # was told is the opposite of the point.
  if [[ "$value" == *=* ]]; then
    echo "   note: '=' in the value of $2 replaced with ':' to keep the record parseable" >&2
    value=${value//=/:}
  fi
  printf '%s=%s\n' "$2" "${value:0:200}" >>"$1"
}

# perf_or_unknown <value> — echo the value, or `unknown` when it is empty.
#
# The record's contract has exactly two absences: a field left out entirely
# ("does not apply to this host") and the literal `unknown` ("could not
# establish"). An empty value is a third state with no defined meaning, and it
# is the one a consumer is most likely to misread — `cores_logical=` reads as
# zero cores, not as a missing `nproc`. Every best-effort rig fact goes through
# here so a missing tool or an unreadable /proc file lands in the vocabulary
# the header documents.
perf_or_unknown() {
  local v=${1-}
  # Trim, then decide: a value of only whitespace is as absent as an empty one.
  v="${v#"${v%%[![:space:]]*}"}"
  v="${v%"${v##*[![:space:]]}"}"
  [[ -n "$v" ]] && printf '%s\n' "$v" || printf 'unknown\n'
}

# perf_record_comment <record> <text> — append a comment line.
perf_record_comment() {
  printf '# %s\n' "$2" >>"$1"
}

# perf_ec2_meta <record> — write instance_type and availability_zone if this is
# an EC2 instance, and nothing at all if it is not.
#
# IMDSv2 (token first) with a one-second timeout. On a devserver the token PUT
# simply fails; that is the expected answer for "not EC2", not an error, so no
# instance fields are written rather than fields reading "unknown". Absent
# means "this box does not describe itself that way".
perf_ec2_meta() {
  local record=$1
  local imds=http://169.254.169.254/latest
  local token type az
  command -v curl >/dev/null 2>&1 || return 0
  token=$(curl -fsS -m 1 -X PUT "$imds/api/token" \
            -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null) || return 0
  [[ -n "$token" ]] || return 0
  type=$(curl -fsS -m 1 -H "X-aws-ec2-metadata-token: $token" \
           "$imds/meta-data/instance-type" 2>/dev/null) || true
  az=$(curl -fsS -m 1 -H "X-aws-ec2-metadata-token: $token" \
         "$imds/meta-data/placement/availability-zone" 2>/dev/null) || true
  [[ -n "$type" ]] && perf_record_kv "$record" instance_type "$type"
  [[ -n "$az" ]]   && perf_record_kv "$record" availability_zone "$az"
  return 0
}

# perf_record_rig <record> — the facts every host can state about itself.
#
# cores/memory/kernel/CPU model are read the same way sampler.sh reads them, so
# the record and the .info sidecar cannot disagree. RIG_NOTE is the free-text
# escape hatch for a host that cannot describe itself (a VM with a generic
# model line, a shared devserver with noisy neighbours worth naming).
perf_record_rig() {
  local record=$1
  perf_record_comment "$record" "rig"
  perf_record_kv "$record" host "$(perf_or_unknown "$(hostname)")"
  perf_record_kv "$record" kernel "$(perf_or_unknown "$(uname -r)")"
  perf_record_kv "$record" arch "$(perf_or_unknown "$(uname -m)")"
  perf_record_kv "$record" cores_logical "$(perf_or_unknown "$(nproc)")"
  perf_record_kv "$record" cores_physical \
    "$(perf_or_unknown "$(lscpu 2>/dev/null | awk -F: '/^Core\(s\) per socket/ {c=$2} /^Socket\(s\)/ {s=$2} END {gsub(/ /,"",c); gsub(/ /,"",s); if (c*s) print c*s}')")"
  perf_record_kv "$record" cpu_model \
    "$(perf_or_unknown "$(awk -F: '/model name/ {gsub(/^ +/, "", $2); print $2; exit}' /proc/cpuinfo 2>/dev/null)")"
  perf_record_kv "$record" mem_total_kb "$(perf_or_unknown "$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null)")"
  perf_ec2_meta "$record"
  [[ -n "${RIG_NOTE:-}" ]] && perf_record_kv "$record" rig_note "$RIG_NOTE"
  return 0
}

# perf_tree_dirty <dir> — `true` when the checkout has uncommitted changes to
# tracked files, `false` when it does not; rc=1 when <dir> is not a checkout.
#
# `--untracked-files=no` is the whole point of this existing. `git status`
# reports the entire repository regardless of the directory it is pointed at,
# so without it an untracked file anywhere in the tree — a scratch note, a
# downloaded toolchain, an editor backup — flips the flag. That is not
# hypothetical: this read `true` for every capture of a 120-run campaign whose
# tree carried no tracked modification at all, and a provenance flag that is
# always on is worse than no flag, because it trains the reader to ignore it.
#
# The cost of ignoring untracked files is real and worth stating: a fixture
# regenerated but never `git add`ed reads clean. For the question the flag
# answers — "was the tracked code at the recorded commit?" — that is the right
# trade, but both callers repeat the caveat where they write the field.
#
# One helper for both callers on purpose. The defect it fixes existed twice,
# in two files, written the same wrong way; a second copy would drift again.
perf_tree_dirty() {
  local out
  out=$(git -C "$1" status --porcelain --untracked-files=no 2>/dev/null) || return 1
  [[ -n "$out" ]] && printf 'true\n' || printf 'false\n'
}

# perf_record_code <record> <repo_root> <bin_dir> <binary>... — what was built.
#
# The commit alone does not identify what ran: a run from a dirty tree is not
# the commit it names, and the binaries under bin/ may predate the checkout
# they sit in (build.sh is a separate step). So record both the commit and the
# hash of each binary actually executed.
perf_record_code() {
  local record=$1 repo_root=$2 bin_dir=$3; shift 3
  local commit dirty b
  perf_record_comment "$record" "code"
  if commit=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null); then
    perf_record_kv "$record" commit "$commit"
    # Tracked changes only — see perf_tree_dirty. An untracked file in the
    # tree is not a difference between this run and the commit it names; a
    # regenerated-but-unadded fixture is the blind spot that buys.
    dirty=$(perf_tree_dirty "$repo_root") || dirty=unknown
    perf_record_kv "$record" commit_dirty "$dirty"
  else
    # A tarball deploy with no .git. Say so rather than leaving the reader to
    # assume the binaries match some commit.
    perf_record_kv "$record" commit unknown
    perf_record_kv "$record" commit_dirty unknown
  fi
  for b in "$@"; do
    if [[ -r "$bin_dir/$b" ]]; then
      perf_record_kv "$record" "${b//-/_}_sha256" "$(sha256sum "$bin_dir/$b" | cut -d' ' -f1)"
    fi
  done
  return 0
}

# perf_record_fixtures <record> <perf_dir> — capture provenance.
#
# The manifest's own hash pins the whole fixture set in one field: the run
# scripts already verified every recorded file against it in their preflight,
# so two runs with the same manifest hash replayed the same bytes. The capture
# date, commit, VPN and RDP come out of the manifest's provenance header.
perf_record_fixtures() {
  local record=$1 perf_dir=$2
  local manifest="$perf_dir/fixtures.manifest"
  perf_record_comment "$record" "fixtures"
  if [[ ! -r "$manifest" ]]; then
    perf_record_kv "$record" fixtures_manifest_sha256 unknown
    return 0
  fi
  perf_record_kv "$record" fixtures_manifest_sha256 "$(sha256sum "$manifest" | cut -d' ' -f1)"
  perf_record_kv "$record" fixtures_files "$(grep -cE '^[0-9a-f]{64}  ' "$manifest" || true)"
  # One awk per key rather than `sed | head`: a `head` that closes the pipe
  # early sends SIGPIPE to sed, and under `pipefail` that fails the pipeline,
  # so the `|| echo unknown` fallback would *append* a second line and write a
  # malformed two-line field into a record everything downstream parses with
  # `awk -F=`. It only bites on a duplicated header key, which is exactly why
  # it would never show up in testing.
  #
  # capture_dirty matters for the same reason commit_dirty does for the run's
  # own tree: a capture taken from a dirty checkout is not the commit it names.
  local k v
  for k in captured_at capture_commit capture_dirty broker_alias vpn rdp; do
    # sub() rather than substr() with a computed offset: there is no arithmetic
    # to get wrong. (The old offset was correct, and perf_or_unknown's trim made
    # an off-by-one invisible anyway — which is exactly why it was not worth a
    # test to pin. Removing the computation beats asserting it.)
    v=$(awk -v key="$k" 'index($0, "# " key ": ") == 1 { sub("^# " key ": ", ""); print; exit }' "$manifest")
    perf_record_kv "$record" "fixtures_$k" "$(perf_or_unknown "$v")"
  done
  return 0
}

# perf_record_admission <record> <mcp_log> <config_used> — the four settings
# that move admission behaviour, each with the source it came from.
#
# Two of these post-date the last full measurement pass and both change
# admission (semp.max_queue_wait, semp.fair_scheduling), so a run that does not
# record them cannot be compared with one that does.
#
# Where the value comes from matters as much as the value, because unset is not
# off: the defaults are a 100ms pacer, 10 in-flight slots, a 30s admission
# bound and fair scheduling ON. A plain YAML parse would therefore report a
# blank — read as "no throttle" by the next person — for any config relying on
# a default.
#
# So each field carries a <key>_source:
#   server-log   the server reported its own effective value at startup
#   config-file  the value was written explicitly in the config the run used
#   unreported-server-default
#                not written down and not reported, so the server applied its
#                default and this harness cannot prove which. Value is
#                `unknown`; a wrong value here is worse than an absent one,
#                because a later comparison would trust it.
#
# Only fair_scheduling is on the `config loaded` line today (it is published
# there deliberately, as a kill switch an operator must be able to confirm).
# Putting the other three on that line is a one-line server change that would
# make all four read server-log — worth doing, but it is production surface and
# this story is scoped to test/performance/.
perf_record_admission() {
  local record=$1 mcp_log=$2 config_used=$3
  perf_record_comment "$record" "admission settings (effective; see lib.sh for what each _source means)"

  # fair_scheduling: the server's own report. JSON logs, so match the key
  # directly rather than depending on field order.
  #
  # Two-stage on purpose. This grep is the harness's only coupling to the
  # server's log output, and it is therefore also the only place that can tell
  # us the coupling broke. Collapsing "the server never logged the line" and
  # "the line is there but the field was renamed" into one `unknown` would let
  # a log-schema change silently degrade every future run forever, with no
  # signal — and the whole justification for reading this value here is that it
  # *is* obtainable.
  local line="" fair=""
  if [[ -r "$mcp_log" ]]; then
    line=$(grep -m1 '"msg":"config loaded"' "$mcp_log" 2>/dev/null) || true
  fi
  if [[ -n "$line" ]]; then
    fair=$(printf '%s' "$line" | sed -n 's/.*"fair_scheduling":\([a-z]*\).*/\1/p')
  fi
  if [[ -n "$fair" ]]; then
    perf_record_kv "$record" semp_fair_scheduling "$fair"
    perf_record_kv "$record" semp_fair_scheduling_source server-log
  elif [[ -n "$line" ]]; then
    echo "   WARNING: the server logged 'config loaded' but no fair_scheduling field —" >&2
    echo "            the log schema changed and lib.sh's reader needs updating." >&2
    perf_record_kv "$record" semp_fair_scheduling unknown
    perf_record_kv "$record" semp_fair_scheduling_source server-log-schema-changed
  else
    perf_record_kv "$record" semp_fair_scheduling unknown
    perf_record_kv "$record" semp_fair_scheduling_source server-log-absent
  fi

  # The remaining three: explicit in the config the run actually used, or not
  # established at all.
  local key val
  for key in max_concurrent_per_broker request_min_interval max_queue_wait; do
    val=""
    if [[ -r "$config_used" ]]; then
      val=$(perf_yaml_semp_value "$config_used" "$key")
    fi
    if [[ -n "$val" ]]; then
      perf_record_kv "$record" "semp_$key" "$val"
      perf_record_kv "$record" "semp_${key}_source" config-file
    elif [[ -r "$config_used" ]] && perf_semp_block "$config_used" | grep -qE "^[[:space:]]*$key:"; then
      # The key is in the file but the narrow reader could not extract it — a
      # shape it does not handle (nesting, flow style, an anchor). Recording
      # `unreported-server-default` here would be a lie about provenance: the
      # value IS written down, we just failed to read it. A wrong _source is in
      # the same family as a wrong value, because a later comparison trusts it.
      echo "   WARNING: semp.$key is set in $(basename "$config_used") but lib.sh could not parse it" >&2
      perf_record_kv "$record" "semp_$key" unknown
      perf_record_kv "$record" "semp_${key}_source" config-file-unparsed
    else
      perf_record_kv "$record" "semp_$key" unknown
      perf_record_kv "$record" "semp_${key}_source" unreported-server-default
    fi
  done
  return 0
}

# perf_semp_block <config> — the lines of the top-level `semp:` block.
#
# The presence probe in perf_record_admission has to be scoped the same way the
# value reader is. Grepping the whole file would let a same-named key under
# another top-level mapping (brokers:, say) mark a setting `config-file-unparsed`
# when it is genuinely a server default — the wrong _source, which is the exact
# class of error the _source scheme exists to prevent.
perf_semp_block() {
  awk '
    # Strip a trailing comment before matching: `semp:  # the pacer` is a legal
    # and unremarkable thing to write, and an exact-match on the raw line made
    # the entire block invisible — so every setting in it was reported as a
    # server default when it was written down. A wrong _source is the class of
    # error the _source scheme exists to prevent.
    /^[^[:space:]#]/ {
      l = $0; sub(/#.*/, "", l); sub(/[[:space:]]+$/, "", l)
      in_semp = (l == "semp:")
      next
    }
    in_semp { print }
  ' "$1"
}

# perf_yaml_semp_value <config> <key> — the value of one scalar key inside the
# top-level `semp:` block, or empty when it is not written down.
#
# Deliberately narrow: it reads exactly the four flat scalars under one known
# top-level key, stops at the next top-level key, and ignores comments. It is
# not a YAML parser and must not grow into one — the moment a caller needs
# nesting or lists, the right answer is for the server to report the value.
perf_yaml_semp_value() {
  awk -v want="$2" '
    # Leaving the semp block: any key at column 0. A trailing comment on the
    # `semp:` line itself is stripped before matching, for the same reason
    # perf_semp_block does it.
    /^[^[:space:]#]/ {
      l = $0; sub(/#.*/, "", l); sub(/[[:space:]]+$/, "", l)
      in_semp = (l == "semp:")
      next
    }
    !in_semp { next }
    {
      line = $0
      sub(/#.*/, "", line)                 # strip trailing comment
      if (line !~ /^[[:space:]]+[A-Za-z_]+:/) next
      k = line; sub(/^[[:space:]]+/, "", k); sub(/:.*/, "", k)
      if (k != want) next
      v = line; sub(/^[^:]*:[[:space:]]*/, "", v)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
      gsub(/^["'\''"]|["'\''"]$/, "", v)
      if (v != "") { print v; exit }
    }
  ' "$1"
}

# perf_record_proc_nofile <record> <pid> — the descriptor limit the sampled
# process actually runs under.
#
# Read from the process's own /proc/<pid>/limits, not from the launching shell:
# the two diverge across a re-exec or a systemd-style wrapper, and the
# process's own view is the one that constrains the run.
perf_record_proc_nofile() {
  local record=$1 pid=$2
  local soft hard
  if [[ -r "/proc/$pid/limits" ]]; then
    soft=$(awk -F'  +' '/^Max open files/ {print $2}' "/proc/$pid/limits")
    hard=$(awk -F'  +' '/^Max open files/ {print $3}' "/proc/$pid/limits")
    perf_record_kv "$record" nofile_effective_soft "${soft:-unknown}"
    perf_record_kv "$record" nofile_effective_hard "${hard:-unknown}"
  else
    perf_record_kv "$record" nofile_effective_soft unknown
    perf_record_kv "$record" nofile_effective_hard unknown
  fi
  return 0
}

# perf_record_assert_fields <record> <field>... — warn loudly when a record is
# closed without a field it should carry. rc=1 when any is missing.
#
# Named fields, never a line count. A count breaks on the next field anyone
# adds and on every comment line, and "a record that is short by one" is
# precisely what no reader notices: a 48-line record looks complete unless you
# count it against another one.
#
# Warns rather than aborts. By the time a record is closed the measurement is
# already in the CSVs, and turning a provenance gap into a non-zero exit would
# discard a completed run over a missing line. The caller decides what to do
# with rc=1; the runners deliberately let it stand as a warning.
perf_record_assert_fields() {
  local record=$1; shift
  local f
  local missing=()
  for f in "$@"; do
    grep -q "^$f=" "$record" 2>/dev/null || missing+=("$f")
  done
  if (( ${#missing[@]} > 0 )); then
    echo "!! incomplete run record ${record##*/}: missing ${missing[*]}" >&2
    return 1
  fi
  return 0
}

# perf_duration_secs <duration> — a Go duration string as whole seconds,
# rounded up. rc=1 with a message on anything else.
#
# Rounded up because every caller sizes a sampler window with the result, and a
# window that ends before the load does clips the tail off the load-phase
# figures — the diluted number the run record exists to stop people quoting.
#
# Deliberately stricter than Go's own parser, which also takes "1m30s" and
# "500ms". The samplers here count in whole seconds, so a duration they cannot
# express is better refused at the door than silently truncated. Pure bash and
# awk arithmetic: no gawk-only 3-argument match(), because this file is sourced
# by the self-test on developer laptops where awk is mawk.
perf_duration_secs() {
  local v=${1-} n unit secs
  if [[ "$v" =~ ^([0-9]+(\.[0-9]+)?)(s|m|h)$ ]]; then
    n=${BASH_REMATCH[1]}
    unit=${BASH_REMATCH[3]}
  else
    echo "duration must be a number of s, m or h (e.g. 30s, 1.5m), got: '$v'" >&2
    return 1
  fi
  # The epsilon is not cosmetic: 1.1 * 3600 is 3960.0000000000005 in floating
  # point, so a bare `s == int(s)` rounds 1.1h up to 3961 seconds.
  #
  # The upper bound stops a fat-fingered value becoming a number that wraps
  # when the caller adds to it: `$(( 1e20 + 10 ))` is negative-adjacent
  # nonsense, and no window this harness sizes is longer than a day.
  secs=$(awk -v n="$n" -v u="$unit" 'BEGIN {
    mult = (u == "h" ? 3600 : (u == "m" ? 60 : 1))
    s = n * mult
    if (s > 86400) { print "over"; exit }
    print (s <= int(s) + 1e-9) ? int(s) : int(s) + 1
  }')
  if [[ "$secs" == over ]]; then
    echo "duration must be 24h or less, got: '$v'" >&2
    return 1
  fi
  printf '%s\n' "$secs"
}

# perf_cgroup_cpu_max <root> <cgroup_path> — the `cpu.max` that binds a process
# in <cgroup_path>, searched from that cgroup upwards to <root>. Echoes the
# file's contents, or nothing when no ancestor sets one.
#
# Split out from perf_record_runtime_cpu so the search is testable with a
# nested path. It is one of the two parts here that could write a confident
# wrong number, and a test that derives its fixture from the *test host's* own
# cgroup cannot exercise it at all on a host whose shell sits at the root —
# leaf and ancestor collapse to one directory and the walk is never taken.
#
# The walk is necessary, not defensive: cpu.max exists only where the cpu
# controller has been enabled, and a limit set on an ancestor still binds.
perf_cgroup_cpu_max() {
  local root=${1%/} dir
  [[ -z "$root" ]] && root=/
  dir="$root${2%/}"
  while :; do
    if [[ -r "$dir/cpu.max" ]]; then
      cat "$dir/cpu.max"
      return 0
    fi
    [[ "$dir" == "$root" || "$dir" == "/" ]] && return 0
    dir=$(dirname "$dir")
  done
}

# perf_cgroup_quota_cores <cpu.max contents> — that quota as a core count:
# `0.25` for "25000 100000", `none` for an unlimited one, `unknown` for a line
# this cannot read.
#
# Cores rather than the raw pair because the raw pair is already recorded next
# to it, and because a reader comparing two runs wants the number the runtime
# would have derived, not two integers to divide by hand.
perf_cgroup_quota_cores() {
  local cpumax=${1-} quota period
  quota=${cpumax%% *}
  period=${cpumax##* }
  if [[ "$quota" == "max" ]]; then
    printf 'none\n'
  elif [[ "$quota" =~ ^[0-9]+$ && "$period" =~ ^[0-9]+$ ]] && (( period > 0 )); then
    awk -v q="$quota" -v p="$period" 'BEGIN { printf "%.2f\n", q / p }'
  else
    printf 'unknown\n'
  fi
}

# perf_record_runtime_cpu <record> <pid> — how much processor the Go runtime in
# <pid> was entitled to, as separately sourced facts.
#
# The record already says everything about the machine (cores_logical,
# cores_physical, cpu_model, instance_type) and nothing about how much of it
# the runtime will use. Two runs on one box with different GOMAXPROCS produce
# records identical in every field, which defeats the purpose the record exists
# for.
#
# Three facts, never one derived "effective GOMAXPROCS", for two reasons:
#
#   * Go 1.25 — what go.mod requires — derives GOMAXPROCS from the cgroup CPU
#     limit when there is one. A field that fell back to nproc would therefore
#     be confidently wrong in exactly the containerised case this exists to
#     detect: deploy/kubernetes/deployment.yaml sets requests.cpu 100m and no
#     CPU limit, so a pod falls back to the node's core count and runs with 64
#     Ps on a 64-core node while entitled to a tenth of a core.
#   * Go 1.25 also updates GOMAXPROCS as cgroup limits change, so no single
#     value read at startup is the whole story anyway.
#
# So: record what can be read, name where each part came from, and leave the
# derivation to the reader — the same rule the admission settings follow.
#
# cgroup v2 only (unified hierarchy, `0::<path>` in /proc/<pid>/cgroup). The
# rig hosts are cgroup2fs throughout; a v1 or hybrid host writes `unknown`
# rather than guessing at a layout it did not read.
perf_record_runtime_cpu() {
  local record=$1 pid=$2
  local env_val cg cpumax
  perf_record_comment "$record" "runtime CPU entitlement (GOMAXPROCS is derived from these, not recorded as one value)"

  # As the process was actually launched, from its own environment — not this
  # shell's, which can differ across a re-exec, and not the value some later
  # reader assumes.
  if [[ -r "/proc/$pid/environ" ]]; then
    # `|| true`, and the redirect's own error silenced: the readability check
    # above and the open below are two moments, and a process that exits
    # between them makes the redirect fail — which, inside a command
    # substitution feeding an assignment, aborts the whole run under `set -e`.
    # Every other best-effort probe here is guarded the same way; this one is
    # in the main flow of two runners, right after the health check.
    env_val=$( { tr '\0' '\n' <"/proc/$pid/environ" || true; } 2>/dev/null \
               | awk -F= '$1 == "GOMAXPROCS" { print substr($0, index($0, "=") + 1); exit }')
    # `unset` is a third value alongside the record's two absences, and it is a
    # measured answer rather than either of them: the variable was looked for
    # and was not there, which is what makes the runtime fall back.
    perf_record_kv "$record" gomaxprocs_env "$(perf_or_unknown "${env_val:-unset}")"
  else
    perf_record_kv "$record" gomaxprocs_env unknown
  fi

  # PERF_CGROUP_ROOT is a test seam and nothing else: the self-test builds a
  # synthetic hierarchy under a temp dir. Unset in every real run, where the
  # root is the mount point and the mount type is checked.
  local cg_root=${PERF_CGROUP_ROOT:-/sys/fs/cgroup}
  cg=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$pid/cgroup" 2>/dev/null) || true
  if [[ -z "$cg" ]] || { [[ -z "${PERF_CGROUP_ROOT:-}" ]] && [[ "$(stat -fc %T "$cg_root" 2>/dev/null)" != cgroup2fs ]]; }; then
    perf_record_kv "$record" cgroup_path unknown
    perf_record_kv "$record" cgroup_cpu_max unknown
    perf_record_kv "$record" cgroup_cpu_quota_cores unknown
    return 0
  fi
  perf_record_kv "$record" cgroup_path "$cg"

  cpumax=$(perf_cgroup_cpu_max "$cg_root" "$cg")
  if [[ -z "$cpumax" ]]; then
    # Reaching the root without finding the file is an answer, not a gap: the
    # v2 root cgroup has no cpu.max by design and imposes no limit, so the
    # runtime sized itself from the visible core count that cores_logical
    # already records. The mount-type check above is what separates this from
    # "we looked in the wrong place".
    perf_record_kv "$record" cgroup_cpu_max none
    perf_record_kv "$record" cgroup_cpu_quota_cores none
    return 0
  fi
  perf_record_kv "$record" cgroup_cpu_max "$cpumax"
  perf_record_kv "$record" cgroup_cpu_quota_cores "$(perf_cgroup_quota_cores "$cpumax")"
  return 0
}

# perf_record_fd_peak <record> <mem_csv> — the highest open-descriptor and
# thread counts the sampler saw.
#
# Appended after the run, because a peak is not knowable at the start. Recorded
# next to the limit so a run that came close to its ceiling is visible
#
# A row whose fd column is "NA" (memsampler could not read the descriptor
# directory) is skipped rather than read as 0, and a run with no readable fd
# sample at all writes no fd_peak field. fd_peak=0 would say the server held no
# descriptors, which is never true and would be believed.
perf_record_fd_peak() {
  local record=$1 csv=$2
  [[ -r "$csv" ]] || return 0
  # Columns resolved by name from the header row, not by index: memsampler
  # owns the layout (see its csvHeader) and this reader must not encode a
  # position that a future column could shift.
  awk -F, -v rec="$record" '
    NR == 1 {
      for (i = 1; i <= NF; i++) { gsub(/^[ \t]+|[ \t]+$/, "", $i); ix[$i] = i }
      tc = ix["threads"]; fc = ix["open_fds"]
      next
    }
    {
      if (tc && $tc ~ /^[0-9]+$/ && $tc+0 > tmax) { tmax = $tc+0; tseen = 1 }
      if (fc && $fc ~ /^[0-9]+$/ && $fc+0 > fmax) { fmax = $fc+0; fseen = 1 }
    }
    END {
      if (tseen) printf "threads_peak=%d\n", tmax >> rec
      if (fseen) printf "fd_peak=%d\n", fmax >> rec
    }
  ' "$csv"
  return 0
}

# ---------------------------------------------------------------------------
# Load phase
# ---------------------------------------------------------------------------

# perf_stamp_load_start <record> / perf_stamp_load_end <record>
#
# The load window is stamped by the runner that drives the load, never inferred
# from the numbers. Inferring it from a CPU threshold would use the metric to
# define the window it is measured over, and would break on exactly the runs
# that matter: a saturated arm and an idle-pacer arm look nothing alike.
#
# Epoch seconds, because summary.sh windows the sampler CSV on its epoch column
# and comparing wall-clock strings would break at midnight.
# The optional second argument is an epoch the caller already read. run.sh
# stamps two records for one load phase and must give both the same instant;
# without it, two `date` calls could disagree by a second and the window a run
# reports would depend on which record summary.sh happened to read first.
perf_stamp_load_start() {
  local epoch=${2:-$(date +%s)}
  perf_record_comment "$1" "load phase (stamped by the runner that drives the load)"
  perf_record_kv "$1" load_start_epoch "$epoch"
  perf_record_kv "$1" load_start_at "$(date -d "@$epoch" -Iseconds)"
}

perf_stamp_load_end() {
  local epoch=${2:-$(date +%s)}
  perf_record_kv "$1" load_end_epoch "$epoch"
  perf_record_kv "$1" load_end_at "$(date -d "@$epoch" -Iseconds)"
  perf_record_kv "$1" load_window_source runner-stamped
}

# perf_no_load_window <record> <why> — declare that this box cannot stamp the
# window, and why.
#
# The split-host MCP box is this case: the load is driven from the other box
# and the halves share no channel by design, so run-mcp.sh genuinely does not
# know when load started. Saying so — and telling the reader how to window the
# numbers once both run directories are archived together — is the honest
# answer. Guessing a boundary would poison every later comparison.
perf_no_load_window() {
  perf_record_comment "$1" "load phase"
  perf_record_kv "$1" load_window_source "$2"
}

# perf_record_begin <record> <role> <run_dir> — start a record.
perf_record_begin() {
  local record=$1 role=$2 run_dir=$3
  : >"$record"
  perf_record_comment "$record" "perf run record — written by the run scripts (test/performance/lib.sh)."
  perf_record_comment "$record" "One key=value per line. 'unknown' means the harness could not establish the"
  perf_record_comment "$record" "value; an absent field means it does not apply to this host."
  perf_record_comment "$record" ""
  # State the classification in the file itself. The fixture identifiers below
  # (vpn, rdp, broker alias) name a real lab appliance, which is why
  # fixtures.manifest is gitignored — this record copies them into a new file
  # that gets shared between engineers and archived, so it has to carry the
  # same warning rather than relying on whoever pastes it to remember.
  perf_record_comment "$record" "INTERNAL: names lab identifiers (vpn/rdp/broker alias) and this host."
  perf_record_comment "$record" "Do not paste into public issues, PRs or pages."
  perf_record_kv "$record" record_version 1
  perf_record_kv "$record" role "$role"
  perf_record_kv "$record" run_dir "$run_dir"
  perf_record_kv "$record" started_at "$(date -Iseconds)"
  perf_record_kv "$record" started_epoch "$(date +%s)"
}
