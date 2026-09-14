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

# perf_redact_userinfo <url> — the URL with any userinfo replaced by `***`.
#
# `http://user:secret@host:9090` becomes `http://***@host:9090`. Nothing in
# this harness produces such a URL — the mock disables client auth entirely and
# loadgen's only auth channel is MCP_DEV_TOKEN, an env var that is never
# persisted — but the URL is caller-supplied, the record is an archived
# artifact that gets shared between engineers, and userinfo is worth nothing as
# provenance. Cheaper to make the shape impossible than to argue about how
# likely it is.
perf_redact_userinfo() {
  local url=${1-}
  # Only the authority component: a `@` later in a path or query is not
  # userinfo, and rewriting it would corrupt the URL the run actually used.
  if [[ "$url" =~ ^([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@]+@(.*)$ ]]; then
    printf '%s***@%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
  else
    printf '%s\n' "$url"
  fi
}

# perf_alias_count <csv> — how many broker aliases a comma-separated list pins.
#
# Empty entries do not count: "a,,b" pins two aliases, not three, and an empty
# string pins none. Split out here rather than inlined in the runner because
# the number it produces goes into the run record as `broker_count`, and a
# provenance field that cannot be tested is how the previous one came to say
# 50 for a run driving three brokers.
perf_alias_count() {
  awk -F, '{ n = 0; for (i = 1; i <= NF; i++) if ($i ~ /[^[:space:]]/) n++; print n }' <<<"${1-}"
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

# perf_yaml_top_value <config> <key> — the value of one scalar key at column 0.
#
# perf_yaml_semp_value's sibling for the top level. Separate rather than
# parameterised because the two have opposite predicates: that one only reads
# inside the `semp:` block, this one only reads outside every block. A
# `log_level` nested under `semp:` is a different setting from the server's own
# log level, and conflating them would report a value the server never applied.
perf_yaml_top_value() {
  awk -v want="$2" '
    {
      line = $0
      sub(/#.*/, "", line)                 # strip trailing comment
      if (line !~ /^[A-Za-z_]+:/) next     # column 0 only
      k = line; sub(/:.*/, "", k)
      if (k != want) next
      v = line; sub(/^[^:]*:[[:space:]]*/, "", v)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
      gsub(/^["'\''"]|["'\''"]$/, "", v)
      if (v != "") { print v; exit }
    }
  ' "$1"
}

# perf_record_log_level <record> <mcp_log> <config_used> — the level the server
# actually ran at, and where that was established.
#
# The level is the input to the volume projection below, so a wrong one
# projects a confident wrong number and either refuses a run that would have
# fit or admits one that fills the disk. It therefore carries a `_source` on
# exactly the same terms as the admission settings, with the same meanings —
# see perf_record_admission for what each label promises.
#
# Unlike those four, this one really is on the `config loaded` line
# (cmd/server/main.go logs log_level beside fair_scheduling), so the common
# path is `server-log`: the server's own report of what it resolved, after its
# own defaults were applied.
perf_record_log_level() {
  local record=$1 mcp_log=$2 config_used=$3
  local line="" level="" source=""

  if [[ -r "$mcp_log" ]]; then
    line=$(grep -m1 '"msg":"config loaded"' "$mcp_log" 2>/dev/null) || true
  fi
  if [[ -n "$line" ]]; then
    level=$(printf '%s' "$line" | sed -n 's/.*"log_level":"\([^"]*\)".*/\1/p')
  fi

  if [[ -n "$level" ]]; then
    source=server-log
  elif [[ -n "$line" ]]; then
    # The line is there and the field is not. Two-stage for the same reason
    # perf_record_admission is: collapsing "never logged" and "field renamed"
    # into one answer would let a log-schema change degrade every future run
    # with no signal. Fall back to the config file for the value, but never
    # claim the server reported it.
    echo "   WARNING: the server logged 'config loaded' but no log_level field —" >&2
    echo "            the log schema changed and lib.sh's reader needs updating." >&2
    source=server-log-schema-changed
    [[ -r "$config_used" ]] && level=$(perf_yaml_top_value "$config_used" log_level)
  else
    [[ -r "$config_used" ]] && level=$(perf_yaml_top_value "$config_used" log_level)
    [[ -n "$level" ]] && source=config-file
  fi

  if [[ -z "$level" ]]; then
    # Not written down and not reported: the server applied its own default and
    # this harness cannot prove which. A wrong value here is worse than an
    # absent one, because the projection would trust it.
    level=unknown
    [[ -z "$source" ]] && source=unreported-server-default
  else
    # Folded to lower case, because the server folds it before validating
    # (internal/config lowercases the configured value) — so `INFO` in a config
    # file is a run at `info`, not a level this harness has never heard of.
    # Without this, a legal config reached the projection as an unknown level,
    # the guard printed "cannot project", and a ten-hour info-level run was
    # admitted — the exact outcome the projection exists to prevent. It also
    # split one campaign's records into INFO and info buckets.
    level=$(printf '%s' "$level" | tr '[:upper:]' '[:lower:]')
  fi

  perf_record_kv "$record" log_level "$level"
  perf_record_kv "$record" log_level_source "$source"
  return 0
}

# perf_human_bytes <bytes> — a byte count for a human, or `unknown`.
#
# Decimal units on purpose. The measurement these projections come from is
# quoted in GB (1.1 GB in 30 minutes), and printing a projection in GiB beside
# a GB measurement invites a reader to think the two disagree by 7% when they
# do not. Anything that is not a plain byte count prints `unknown` rather than
# a number derived from nothing.
perf_human_bytes() {
  local b=${1:-}
  [[ "$b" =~ ^[0-9]+$ ]] || { printf 'unknown\n'; return 0; }
  awk -v b="$b" 'BEGIN {
    if (b < 1000)          { printf "%d B\n", b }
    else if (b < 1000000)  { printf "%.1f KB\n", b / 1000 }
    else if (b < 1000000000) { printf "%.1f MB\n", b / 1000000 }
    else if (b < 1000000000000) { printf "%.1f GB\n", b / 1000000000 }
    else                   { printf "%.1f TB\n", b / 1000000000000 }
  }'
}

# PERF_LOG_BYTES_PER_HOUR_INFO — the one log-volume rate that was ever measured.
#
# SOL-154158's 30-minute control at log_level info: 1.1 GB and 4,512,819 lines
# at 2,507 calls/s, so ~2.2 GB/h. RSS was unaffected (174.4 vs 174.6 MB) — this
# is a disk failure mode, not a memory one, and a full disk kills the run and
# its samplers together.
#
# It scales with the call rate, and the MCP box does not know the call rate:
# the load runs on the other box by design. So this is a projection at roughly
# 2,500 calls/s and nothing more, and every caller prints that caveat beside
# the number rather than presenting it as a prediction.
PERF_LOG_BYTES_PER_HOUR_INFO=2200000000

# PERF_LOG_VOLUME_MAX_FRACTION — refuse when the projection exceeds this much
# of the free space on the log volume. Half, because the run directory also
# takes the sampler CSVs, the fidelity capture and the mock's own log, and a
# projection that is right to within a factor of two is still useful at half.
PERF_LOG_VOLUME_MAX_FRACTION=50

# perf_project_log_volume <level> <duration_secs> <avail_bytes> — echoes
# "<projected_bytes> <verdict> <basis>".
#
#   verdict  ok       the projection fits within the threshold
#            over     it does not, and the caller should refuse
#            unknown  it could not be projected — an unestablished or unknown
#                     level, or free space the caller could not read
#   basis    measured the rate came from a real measurement at this level
#            floor    the rate is a lower bound; the real figure is higher
#            shed-dependent
#                     no per-call line while healthy, but one per shed request
#                     under admission pressure — not projectable from here
#            none     nothing was projected
#
# Pure arithmetic over its three arguments so it can be tested without a server
# or a disk. `unknown` never refuses: blocking a campaign over a value nobody
# measured is a worse failure than letting a run fill a volume the operator can
# see the projection for.
perf_project_log_volume() {
  local level=$1 secs=$2 avail=$3
  local rate basis projected

  case "$level" in
    info)         rate=$PERF_LOG_BYTES_PER_HOUR_INFO; basis=measured ;;
    # Never measured, and cannot be quieter than info — it adds lines, it does
    # not remove them. info's rate is therefore a floor, and the label says so
    # so nobody quotes the number as a measurement.
    debug)        rate=$PERF_LOG_BYTES_PER_HOUR_INFO; basis=floor ;;
    # No per-call line at these levels in a *healthy* run — the SOL-154158
    # control measured startup lines and nothing else. But the shed and
    # slow-admission paths log at warn once per SEMP request
    # (internal/semp/resilience/sender.go: "request shed: broker admission
    # bound exceeded", "broker admission slow"), and driving a broker past
    # semp.max_concurrent_per_broker is precisely what this harness's admission
    # knobs exist to do. At 2,000 shed/s those lines outweigh the info figure
    # this guard refuses on.
    #
    # So volume here is a function of the shed rate, which this box cannot
    # know, and the honest answer is that it cannot be projected — not a
    # confident zero that would admit the run that fills the disk at hour
    # three.
    warn|error)   printf '0 unknown shed-dependent\n'; return 0 ;;
    *)            printf '0 unknown none\n'; return 0 ;;
  esac

  if [[ ! "$secs" =~ ^[0-9]+$ ]] || (( secs <= 0 )); then
    # `unknown`, not `ok`: nothing was projected, and the contract above
    # reserves ok/over for a figure that was. Both runners validate DURATION
    # long before reaching here, so this is a contract guarantee for the next
    # caller rather than a path in use today.
    printf '0 unknown none\n'
    return 0
  fi

  projected=$(( rate * secs / 3600 ))

  # Free space the caller could not read is not "no space" and not "infinite
  # space" — it is no answer, and there is nothing to compare against.
  if [[ ! "$avail" =~ ^[0-9]+$ ]] || (( avail <= 0 )); then
    printf '%s unknown %s\n' "$projected" "$basis"
    return 0
  fi

  if (( projected * 100 > avail * PERF_LOG_VOLUME_MAX_FRACTION )); then
    printf '%s over %s\n' "$projected" "$basis"
  else
    printf '%s ok %s\n' "$projected" "$basis"
  fi
  return 0
}

# perf_guard_log_volume <record> <mcp_log> <config_used> <runs_dir> <secs>
# — record the effective log level, print the projected log volume, and refuse
# a run that cannot fit. Returns non-zero when the caller should stop.
#
# Both runners call this, because both can fill the volume: the skill-side soak
# driver already refuses an info-level config for a long run and the repo
# runners did not. The projection is printed on every path, including the ones
# that do not refuse — the number is useful even when it is comfortable, and it
# is the only place the run says what its logging is expected to cost.
#
# ALLOW_VERBOSE_LOGS=1 overrides the refusal. Deliberately an override and not
# a threshold knob: someone who wants 22 GB of logs on purpose says so once,
# rather than tuning a fraction until the check passes.
#
# PERF_LOG_AVAIL_BYTES is a test seam and nothing else. A real run reads df on
# the run directory; no test can control that without filling a disk.
perf_guard_log_volume() {
  local record=$1 mcp_log=$2 config_used=$3 runs_dir=$4 secs=$5
  # Where the load runs, which decides only whether the printed caveat blames
  # the other box for the unknown call rate. "local" for the single-host
  # runner, where the generator is on this box; "remote" (the default) for the
  # split-host MCP box, which genuinely cannot know the rate.
  local locality=${6:-remote}
  local level src avail proj verdict basis

  perf_record_log_level "$record" "$mcp_log" "$config_used"
  level=$(awk -F= '/^log_level=/ {print $2; exit}' "$record")
  src=$(awk -F= '/^log_level_source=/ {print $2; exit}' "$record")

  if [[ -n "${PERF_LOG_AVAIL_BYTES:-}" ]]; then
    avail=$PERF_LOG_AVAIL_BYTES
  else
    # Free space on the volume the log is actually written to, not on $PWD.
    # `|| true` and the digit filter: an unreadable df is no answer, and it
    # must not arrive as an empty string that later reads as a full disk.
    avail=$( { df -B1 --output=avail "$runs_dir" 2>/dev/null || true; } | tail -1 | tr -dc '0-9')
    # `--output` is GNU coreutils only. Without the fallback the guard degrades
    # to advisory-only on a busybox or BSD jump box — it still prints, but the
    # refusal AC5 asks for is quietly gone. POSIX `df -k` is 1K blocks, avail
    # in column 4.
    if [[ -z "$avail" ]]; then
      # -P: POSIX output, one line per filesystem. Without it a long device
      # name wraps onto its own line, the data line has five fields, and $4 is
      # the capacity percentage — which fails the digit test and leaves the
      # guard advisory-only on exactly the hosts this fallback exists for.
      avail=$( { df -Pk "$runs_dir" 2>/dev/null || true; } | tail -1 \
               | awk '{ if ($4 ~ /^[0-9]+$/) printf "%d", $4 * 1024 }')
    fi
  fi

  read -r proj verdict basis <<<"$(perf_project_log_volume "$level" "$secs" "${avail:-0}")"

  echo "   log volume: level=$level ($src) over ${secs}s"
  # One branch per basis, rather than splicing the basis token into one
  # sentence written for `measured`. Spliced, a warn-level run printed
  # "projected ~0 B (none at ~2,500 calls/s ...)", which is not a sentence and
  # told the operator the opposite of what the zero meant.
  case "$basis" in
    shed-dependent)
      echo "               cannot project at this level: no per-call line while healthy,"
      echo "               but one per shed request under admission pressure — and the"
      echo "               shed rate is not knowable here. Watch the volume yourself."
      ;;
    none)
      echo "               cannot project — the level was not established ($src)"
      ;;
    *)
      printf '               projected ~%s, %s at ~2,500 calls/s\n' \
        "$(perf_human_bytes "$proj")" "$basis"
      if [[ "$locality" == remote ]]; then
        echo "               (scales with the call rate, which this box does not know —"
        echo "                the load runs on the other box)"
      else
        echo "               (scales with the call rate)"
      fi
      # gctrace goes to the same mcp.log when GODEBUG is set for a run, and it
      # is not in this projection: the README recommends the flag for exactly
      # the long runs this guard is sizing, so say what is excluded instead of
      # modelling a second rate nobody measured.
      echo "               Server log lines only: GODEBUG=gctrace output lands in the"
      echo "               same file and is not counted (~190 B per GC cycle)."
      ;;
  esac
  if [[ "$basis" != none && "$basis" != shed-dependent ]]; then
    if [[ -n "${avail:-}" ]] && (( avail > 0 )); then
      printf '               free on the log volume: %s   threshold: %s%%\n' \
        "$(perf_human_bytes "$avail")" "$PERF_LOG_VOLUME_MAX_FRACTION"
    else
      echo "               free space on the log volume could not be read"
    fi
  fi

  if [[ "$verdict" == over ]]; then
    if [[ "${ALLOW_VERBOSE_LOGS:-}" == "1" ]]; then
      echo "               ALLOW_VERBOSE_LOGS=1 — running anyway"
      return 0
    fi
    echo "   REFUSING: the projected log volume exceeds ${PERF_LOG_VOLUME_MAX_FRACTION}% of the free space" >&2
    echo "             on the log volume. Lower log_level in the config this run uses," >&2
    echo "             shorten the run, free space, or set ALLOW_VERBOSE_LOGS=1 to run anyway." >&2
    return 1
  fi
  return 0
}

# perf_filter_godebug — reduce GODEBUG to the one setting this harness asks
# for, in place, before any child is launched.
#
# The runners capture their children's stderr into archived files: mcp.log,
# loadgen.log, fidelity.log, mock.log. GODEBUG is a general knob, and
# `http2debug=2` makes the Go HTTP/2 client print every frame it sends —
# including the `Authorization` header that loadgen and fidelity set from
# MCP_DEV_TOKEN, and that the MCP server sends to the broker. Raw
# Authorization values are on the never-log list in
# docs/internal/secure-logging-rules.md, and slog's ReplaceAttr safety net
# cannot reach them: that output comes from the runtime's own printf path.
#
# Enforced rather than documented, which is the rule the rest of this file
# follows — mcp_url has its userinfo stripped before it reaches a record, and
# perf_record_kv substitutes `=` rather than asking callers not to type one.
#
# gctrace=0 is dropped too, with its own warning: it passes a naive
# `^gctrace=` filter, produces no output, and would cost an operator a
# ten-hour soak's live-heap series they believed they had captured.
perf_filter_godebug() {
  [[ -z "${GODEBUG:-}" ]] && return 0
  local kept
  kept=$(printf '%s' "$GODEBUG" | tr ',' '\n' | grep -E '^gctrace=[1-9]' | paste -sd, -) || true
  if [[ "$kept" != "$GODEBUG" ]]; then
    echo "   WARNING: GODEBUG reduced to '${kept:-<empty>}' — this runner captures child" >&2
    echo "            stderr into archived logs, and non-gctrace values (http2debug) write" >&2
    echo "            request headers into them. gctrace=0 is dropped as a no-op too." >&2
  fi
  if [[ -n "$kept" ]]; then
    export GODEBUG="$kept"
  else
    unset GODEBUG
  fi
  return 0
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
# "500ms". This harness counts in whole seconds — sampler windows are sized in
# them and `stats_start_epoch` is one — so a duration that cannot be expressed
# in them is refused at the door rather than silently rounded into one. Pure bash and
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
  # A duration is accepted only when it lands on a whole second. Rounding into
  # one looked harmless — it sizes a sampler window, where a second either way
  # is nothing — but the same resolved value also positions `stats_start_epoch`,
  # and an epoch is a point, not a span: WARMUP=0.5s would place the statistics
  # window half a second away from where loadgen actually opened it, silently.
  # Fractional inputs that *do* land whole are fine (1.5m is 90s exactly), so
  # this rejects the ones that cannot be represented rather than the notation.
  #
  # The epsilon is not cosmetic: 1.1 * 3600 is 3960.0000000000005 in floating
  # point, so a bare `s == int(s)` would reject 1.1h as fractional.
  #
  # The upper bound stops a fat-fingered value becoming a number that wraps
  # when the caller adds to it: `$(( 1e20 + 10 ))` is negative-adjacent
  # nonsense, and no window this harness sizes is longer than a day.
  secs=$(awk -v n="$n" -v u="$unit" 'BEGIN {
    mult = (u == "h" ? 3600 : (u == "m" ? 60 : 1))
    s = n * mult
    if (s > 86400) { print "over"; exit }
    if (s - int(s) > 1e-9 && int(s) + 1 - s > 1e-9) { print "fractional"; exit }
    printf "%d\n", (s + 1e-9)
  }')
  case "$secs" in
    over)
      echo "duration must be 24h or less, got: '$v'" >&2
      return 1 ;;
    fractional)
      echo "duration must be a whole number of seconds, got: '$v'" >&2
      return 1 ;;
  esac
  printf '%s\n' "$secs"
}

# perf_cgroup_quota_cores <cpu.max contents> — that quota as a core count:
# `0.25` for "25000 100000", `none` for an unlimited one, `unknown` for a line
# this cannot read.
#
# The two-field shape is validated before either branch. A truncated file
# holding just "25000" would otherwise pass `${cpumax%% *}` and `${cpumax##* }`
# as the same token and report a confident 1.00 core, and a bare "max" would
# report `none` — both of them entitlements nobody measured. `unknown` is the
# contract for a line that does not parse.
perf_cgroup_quota_cores() {
  local cpumax=${1-} quota period
  # Exactly two whitespace-separated fields, or it is not a cpu.max.
  if [[ ! "$cpumax" =~ ^[^[:space:]]+[[:space:]]+[^[:space:]]+$ ]]; then
    printf 'unknown\n'
    return 0
  fi
  quota=${cpumax%%[[:space:]]*}
  period=${cpumax##*[[:space:]]}
  if [[ "$quota" == "max" && "$period" =~ ^[0-9]+$ ]]; then
    printf 'none\n'
  elif [[ "$quota" =~ ^[0-9]+$ && "$period" =~ ^[0-9]+$ ]] && (( period > 0 )); then
    awk -v q="$quota" -v p="$period" 'BEGIN { printf "%.2f\n", q / p }'
  else
    printf 'unknown\n'
  fi
}

# perf_cgroup_binding_quota <root> <cgroup_path> — the CPU quota that actually
# constrains a process in <cgroup_path>, as "<cores> <cgroup> <cpu.max>".
# Echoes nothing when no cgroup on the path sets one.
#
# Every cgroup from the process's own up to the root is read, not just the
# nearest with a cpu.max, because v2 CPU limits are hierarchical: a parent's
# bandwidth bounds its whole subtree, so a 2-core leaf under a half-core parent
# gets half a core. Reporting the nearest file would have overstated that by
# fourfold — a confident wrong number, which is the one thing this record is
# built not to produce.
#
# The most restrictive wins, and the cgroup that set it is returned alongside,
# so the record can name what binds rather than leaving the reader to guess
# which level it came from. An unparsable cpu.max on the path is skipped rather
# than treated as unlimited: a file we cannot read is not permission.
perf_cgroup_binding_quota() {
  local root=${1%/} dir cores best="" best_dir="" best_raw="" raw
  [[ -z "$root" ]] && root=/
  dir="$root${2%/}"
  while :; do
    if [[ -r "$dir/cpu.max" ]]; then
      # `|| true` with the redirect silenced: the -r test above and this read
      # are two moments, and a cgroup torn down between them (a server dying
      # during startup, a scope removed) makes cat fail — which inside a
      # command substitution aborts the caller under set -e, mid-record.
      raw=$( { cat "$dir/cpu.max" || true; } 2>/dev/null )
      cores=$(perf_cgroup_quota_cores "$raw")
      if [[ "$cores" != none && "$cores" != unknown ]]; then
        if [[ -z "$best" ]] || awk -v a="$cores" -v b="$best" 'BEGIN { exit !(a < b) }'; then
          best=$cores
          best_raw=$raw
          # Report the cgroup relative to the root, which is what
          # /proc/<pid>/cgroup names.
          best_dir=${dir#"$root"}
          [[ -z "$best_dir" ]] && best_dir=/
        fi
      fi
    fi
    [[ "$dir" == "$root" || "$dir" == "/" ]] && break
    dir=$(dirname "$dir")
  done
  [[ -n "$best" ]] && printf '%s %s %s\n' "$best" "$best_dir" "$best_raw"
  return 0
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

  local binding cores from
  binding=$(perf_cgroup_binding_quota "$cg_root" "$cg")
  cpumax=""
  if [[ -n "$binding" ]]; then
    cores=${binding%% *}
    from=$(printf '%s' "$binding" | cut -d' ' -f2)
    cpumax=$(printf '%s' "$binding" | cut -d' ' -f3-)
  fi
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
  perf_record_kv "$record" cgroup_cpu_quota_cores "$cores"
  # Which cgroup on the path actually binds. Equal to cgroup_path when the
  # process's own cgroup is the tightest; an ancestor otherwise, and that
  # difference is the whole reason the walk reads every level.
  perf_record_kv "$record" cgroup_cpu_quota_from "$from"
  return 0
}

# perf_cgroup_binding_memory <root> <cgroup_path> — the memory limit that
# actually binds, echoed as "<cgroup> <raw>", empty when nothing sets one.
#
# Same hierarchy rule as cpu.max and the same reason for walking it: a v2
# parent's limit bounds its whole subtree, so a 1 GiB leaf under a 256 MiB
# parent gets 256 MiB and reporting the leaf would overstate it fourfold.
#
# memory.max is one token, unlike cpu.max's two: a byte count, or `max` for no
# limit. `max` is an answer and is skipped as "no limit here"; anything that is
# neither `max` nor a plain byte count is skipped as unread rather than treated
# as permission — the same rule perf_cgroup_quota_cores applies to a cpu.max
# line it cannot parse. No derived value is echoed: bytes are bytes, and a
# reader who wants MiB can divide. The caller labels the absence.
perf_cgroup_binding_memory() {
  local root=${1%/} dir best="" best_dir="" raw
  [[ -z "$root" ]] && root=/
  dir="$root${2%/}"
  while :; do
    if [[ -r "$dir/memory.max" ]]; then
      # `|| true` with the redirect silenced: the -r test above and this read
      # are two moments, and a cgroup torn down between them (a server dying
      # during startup, a scope removed) makes cat fail — which inside a
      # command substitution aborts the caller under set -e, mid-record.
      raw=$( { cat "$dir/memory.max" || true; } 2>/dev/null )
      # A trailing newline is already stripped by the substitution; what is
      # left must be all digits. `max`, an empty file and a partial write all
      # fail this and are skipped.
      if [[ "$raw" =~ ^[0-9]+$ ]]; then
        if [[ -z "$best" ]] || (( raw < best )); then
          best=$raw
          # Relative to the root, which is what /proc/<pid>/cgroup names.
          best_dir=${dir#"$root"}
          [[ -z "$best_dir" ]] && best_dir=/
        fi
      fi
    fi
    [[ "$dir" == "$root" || "$dir" == "/" ]] && break
    dir=$(dirname "$dir")
  done
  [[ -n "$best" ]] && printf '%s %s\n' "$best_dir" "$best"
  return 0
}

# perf_record_runtime_mem <record> <pid> — how much memory the Go runtime in
# <pid> was entitled to, as separately sourced facts.
#
# The memory half of perf_record_runtime_cpu, and it exists for a measured
# reason: the SOL-154158 GOMEMLIMIT ladder ran four arms differing only in that
# variable and produced records identical in every field, distinguishable only
# by a tag typed in by hand. A campaign that cannot tell its own arms apart
# from the artefacts cannot be re-read later.
#
# Two independent facts, never one derived "effective limit", for the same
# reason the CPU side refuses to derive GOMAXPROCS:
#
#   * GOMEMLIMIT is a soft target the runtime honours by collecting harder;
#     cgroup memory.max is a hard wall the kernel enforces by killing. They are
#     different mechanisms with different failure modes, and a single number
#     would hide which one a run was actually up against.
#   * Either can be absent, and absence is a measured answer in both cases.
#
# cgroup v2 only, same as the CPU fields: a v1 or hybrid host writes `unknown`
# rather than guessing at a layout it did not read. cgroup_path is deliberately
# not written here — perf_record_runtime_cpu already records it for this same
# pid, and a record with the key twice is a record a parser has to guess at.
perf_record_runtime_mem() {
  local record=$1 pid=$2
  local env_val cg binding
  perf_record_comment "$record" "runtime memory entitlement (GOMEMLIMIT is a soft target, cgroup memory.max a hard wall — separate facts)"

  # As the process was actually launched, from its own environment. Guarded the
  # same way the CPU side is: the readability check and the open below are two
  # moments, and a process that exits between them makes the redirect fail,
  # which inside a command substitution aborts the whole run under `set -e`.
  if [[ -r "/proc/$pid/environ" ]]; then
    env_val=$( { tr '\0' '\n' <"/proc/$pid/environ" || true; } 2>/dev/null \
               | awk -F= '$1 == "GOMEMLIMIT" { print substr($0, index($0, "=") + 1); exit }')
    # `unset` is a measured third value, not an absence: the variable was
    # looked for and was not there, which is what leaves the runtime with no
    # soft target at all.
    perf_record_kv "$record" gomemlimit_env "$(perf_or_unknown "${env_val:-unset}")"
  else
    perf_record_kv "$record" gomemlimit_env unknown
  fi

  # PERF_CGROUP_ROOT is a test seam and nothing else; unset in every real run,
  # where the root is the mount point and the mount type is checked.
  local cg_root=${PERF_CGROUP_ROOT:-/sys/fs/cgroup}
  cg=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$pid/cgroup" 2>/dev/null) || true
  if [[ -z "$cg" ]] || { [[ -z "${PERF_CGROUP_ROOT:-}" ]] && [[ "$(stat -fc %T "$cg_root" 2>/dev/null)" != cgroup2fs ]]; }; then
    perf_record_kv "$record" cgroup_memory_max unknown
    perf_record_kv "$record" cgroup_memory_max_from unknown
    return 0
  fi

  binding=$(perf_cgroup_binding_memory "$cg_root" "$cg")
  if [[ -z "$binding" ]]; then
    # Reaching the root without finding a byte count is an answer, not a gap:
    # the v2 root cgroup sets no memory.max by design, so nothing on the path
    # caps this process and mem_total_kb on the rig line is the only ceiling.
    # The mount-type check above is what separates this from "we looked in the
    # wrong place".
    perf_record_kv "$record" cgroup_memory_max none
    perf_record_kv "$record" cgroup_memory_max_from none
    return 0
  fi
  perf_record_kv "$record" cgroup_memory_max "$(printf '%s' "$binding" | cut -d' ' -f2-)"
  # Which cgroup on the path actually binds. Equal to cgroup_path when the
  # process's own cgroup is the tightest; an ancestor otherwise, and that
  # difference is the whole reason the walk reads every level.
  perf_record_kv "$record" cgroup_memory_max_from "$(printf '%s' "$binding" | cut -d' ' -f1)"
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
