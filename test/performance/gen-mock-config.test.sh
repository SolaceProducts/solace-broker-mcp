#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# Self-test for gen-mock-config.sh.
#
# The dangerous failure here is not a crash, it is a config that starts fine
# and measures nothing: if the generated aliases differ from the ones loadgen
# generates, every tool call 404s at the mock and the run reads like a broker
# or server fault rather than a naming mismatch. So the alias scheme gets three
# assertions of its own, at one, two and three digits, plus a guard on
# loadgen's format string so a change there fails here instead of in a
# four-hour >50-broker run.
#
# The anchor case is N=50 against the committed broker-config.mock.yaml. That
# file is known-good — it is what starts the server today — so reproducing its
# brokers block byte-for-byte proves the generated shape, the alias format and
# the port derivation all at once, against a reference nobody had to invent.
#
# Usage: ./gen-mock-config.test.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN="$here/gen-mock-config.sh"
COMMITTED="$here/broker-config.mock.yaml"
LOADGEN_SRC="$here/loadgen/main.go"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() {   printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad() {  printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }

# indent <file> [max-lines] — echo a file under the failure that names it.
#
# awk rather than `sed | head`: a `head` that closes the pipe early sends
# SIGPIPE to sed, which under `set -o pipefail` aborts this script — so the
# first real failure would kill the report instead of printing it. A test
# harness that cannot narrate its own failures is worse than no harness.
indent() {
  awk -v max="${2:-12}" 'NR <= max { print "        " $0 }' "$1"
}

check() { # <name> <condition-description-already-evaluated:0|1>
  if [[ "$2" == 0 ]]; then ok "$1"; else bad "$1"; fi
}

# assert_rc <name> <want_rc> <args...>
assert_rc() {
  local name=$1 want=$2; shift 2
  local rc=0
  "$GEN" "$@" >"$tmp/out" 2>"$tmp/err" || rc=$?
  if [[ "$rc" == "$want" ]]; then
    ok "$name"
  else
    bad "$name (rc=$rc, want $want)"
    indent "$tmp/err" 3
  fi
}

echo "== the committed 50-broker config is the reference"

# 1. Byte-identical brokers block at N=50. Everything the generator has to get
#    right about a broker line — alias, port, placeholders, YAML flow mapping,
#    two-space indent — is in this one comparison.
"$GEN" -n 50 -o "$tmp/gen50.yaml" >/dev/null
if diff -u <(sed -n '/^brokers:/,$p' "$COMMITTED") \
           <(sed -n '/^brokers:/,$p' "$tmp/gen50.yaml") >"$tmp/diff50"; then
  ok "N=50 reproduces the committed brokers block byte-for-byte"
else
  bad "N=50 diverges from the committed brokers block"
  indent "$tmp/diff50" 12
fi

# 1b. The block ABOVE `brokers:` is a second copy of the committed config's
#     head — the four semp: admission scalars, port, mcp_client_auth, the tls_*
#     fields. The diff above starts at `^brokers:` and so covered none of it,
#     which left a future edit to the committed semp: block landing in one file
#     only. The two arms of a 50-vs-200-broker campaign would then differ in an
#     admission setting as well as in broker count.
#
#     Compared comment-stripped, because the comment blocks are *meant* to
#     differ: the generated file carries its own provenance header and a
#     shortened pacer note. Every semantic line must match.
strip_semantic() { # <file> — the pre-brokers region, comments and blanks removed
  sed -n '1,/^brokers:/p' "$1" | sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d'
}
if diff -u <(strip_semantic "$COMMITTED") \
           <(strip_semantic "$tmp/gen50.yaml") >"$tmp/diffhead"; then
  ok "N=50 reproduces the committed config's settings above brokers: (comments aside)"
else
  bad "the generated config's settings diverge from the committed config"
  indent "$tmp/diffhead" 12
fi

echo "== alias scheme matches loadgen's generated names"

# 2. loadgen builds its alias list with "%s-%02d" (resolveBrokers). %02d is a
#    *minimum* width, so 9 -> broker-09 but 100 -> broker-100. If that literal
#    ever changes, this test must fail rather than the run.
if grep -q '"%s-%02d"' "$LOADGEN_SRC"; then
  ok "loadgen still generates aliases as \"%s-%02d\""
else
  bad "loadgen's alias format string changed — gen-mock-config.sh must follow it"
  grep -n '%s-%0' "$LOADGEN_SRC" >"$tmp/fmt" || true
  indent "$tmp/fmt" 3
fi

# 3. One, two and three digits. The three-digit case is the one a plausibly
#    wrong implementation breaks: padding to the width of N would emit
#    broker-001 for a 200-broker run and rename all 99 of the first brokers.
"$GEN" -n 9   -o "$tmp/gen9.yaml"   >/dev/null
"$GEN" -n 200 -o "$tmp/gen200.yaml" >/dev/null

has() { grep -qE "^  $2: " "$1"; }
lacks() { ! grep -qE "^  $2: " "$1"; }

for want in broker-01 broker-09; do
  if has "$tmp/gen9.yaml" "$want"; then ok "N=9 emits $want"; else bad "N=9 missing $want"; fi
done
if lacks "$tmp/gen9.yaml" broker-10; then ok "N=9 stops at broker-09"; else bad "N=9 emitted broker-10"; fi

for want in broker-01 broker-09 broker-10 broker-99 broker-100 broker-200; do
  if has "$tmp/gen200.yaml" "$want"; then ok "N=200 emits $want"; else bad "N=200 missing $want"; fi
done
for unwanted in broker-009 broker-0100 broker-201; do
  if lacks "$tmp/gen200.yaml" "$unwanted"; then
    ok "N=200 does not emit $unwanted"
  else
    bad "N=200 emitted $unwanted — width was padded to N instead of %02d"
  fi
done

# 4. Exactly N brokers, no more. A trailing extra or a fencepost error at the
#    top of the range would still pass every spot check above.
for n in 9 50 200; do
  # `|| true`: grep -c exits 1 on zero matches, and under `set -e` that would
  # kill this script mid-report on exactly the worst regression — a generator
  # that emitted no broker lines at all would abort the run instead of printing
  # a FAIL. Same reason indent() avoids `sed | head`.
  got=$(grep -cE '^  broker-[0-9]+: ' "$tmp/gen$n.yaml" || true)
  if [[ "$got" == "$n" ]]; then
    ok "N=$n emits exactly $n broker entries"
  else
    bad "N=$n emitted $got broker entries"
  fi
done

echo "== environment placeholders survive verbatim"

# 5. MCP hard-fails on an unset ${VAR}, so a generator that expanded or dropped
#    one produces a config that cannot start. Count them: one of each per
#    broker line, unexpanded.
for n in 9 200; do
  bad_ph=0
  for ph in 'MOCK_HOST' 'BROKER_USERNAME' 'BROKER_PASSWORD'; do
    got=$(grep -cF "\${$ph}" "$tmp/gen$n.yaml" || true)
    [[ "$got" == "$n" ]] || { bad "N=$n has $got \${$ph} placeholders, want $n"; bad_ph=1; }
  done
  (( bad_ph )) || ok "N=$n preserves all three \${...} placeholders on every broker line"
done

echo "== ports derive from -port-start"

"$GEN" -n 3 -port-start 20000 -o "$tmp/ports.yaml" >/dev/null
if grep -q 'broker-01: .*:20000"' "$tmp/ports.yaml" &&
   grep -q 'broker-03: .*:20002"' "$tmp/ports.yaml"; then
  ok "-port-start 20000 maps broker-01..03 onto 20000..20002"
else
  bad "-port-start did not map onto consecutive ports"
  indent "$tmp/ports.yaml"
fi

"$GEN" -n 2 -prefix mock -o "$tmp/prefix.yaml" >/dev/null
if grep -qE '^  mock-01: ' "$tmp/prefix.yaml" && grep -qE '^  mock-02: ' "$tmp/prefix.yaml"; then
  ok "-prefix mock emits mock-01..mock-02"
else
  bad "-prefix was not honoured"
fi

echo "== the committed config is protected"

# 6. By name, by absolute path, and through a symlink — all the same file, and
#    all must be refused. Nobody should commit a 200-broker config, and losing
#    the committed 50-broker one to a stray -o is a silent, annoying loss.
before_hash=$(sha256sum "$COMMITTED" | cut -d' ' -f1)
assert_rc "refuses -o ./broker-config.mock.yaml"           2 -n 5 -o "$here/broker-config.mock.yaml"
assert_rc "refuses it even with -f"                        2 -n 5 -f -o "$here/broker-config.mock.yaml"
ln -s "$COMMITTED" "$tmp/link.yaml"
assert_rc "refuses a symlink to the committed config"      2 -n 5 -o "$tmp/link.yaml"
after_hash=$(sha256sum "$COMMITTED" | cut -d' ' -f1)
check "the committed config is unmodified after the refusals" \
  "$([[ "$before_hash" == "$after_hash" ]] && echo 0 || echo 1)"

echo "== overwrite protection and argument validation"

assert_rc "refuses an existing output without -f"          2 -n 5 -o "$tmp/gen50.yaml"
assert_rc "overwrites an existing output with -f"          0 -n 5 -f -o "$tmp/gen50.yaml"
assert_rc "rejects -n 0"                                   2 -n 0 -o "$tmp/x.yaml"
assert_rc "rejects a non-numeric -n"                       2 -n abc -o "$tmp/x.yaml"
assert_rc "rejects a missing -n"                           2 -o "$tmp/x.yaml"
assert_rc "rejects a missing -o"                           2 -n 5
assert_rc "rejects an unknown argument"                     2 -n 5 -o "$tmp/x.yaml" -wat
assert_rc "rejects a port range past 65535"                2 -n 100 -port-start 65500 -o "$tmp/x.yaml"
assert_rc "rejects a non-numeric -port-start"              2 -n 5 -port-start abc -o "$tmp/x.yaml"
assert_rc "rejects a prefix with a space"                  2 -n 5 -prefix 'bad name' -o "$tmp/x.yaml"
assert_rc "rejects -o into a directory that does not exist" 1 -n 5 -o "$tmp/nope/x.yaml"

# The mock's control endpoint is a fixed :19000, inside the broker range from
# -n 920. A collision is not loud: that broker answers /_mock/config instead of
# SEMP, so the run 404s on one broker and reads as a broker fault.
assert_rc "rejects a range spanning the mock control port"  2 -n 920 -o "$tmp/x.yaml"
assert_rc "accepts -n 919, which stops just below it"       0 -n 919 -o "$tmp/big.yaml"

# 7. A rejected run must leave nothing behind — not the output, and not the
#    temp file it writes through.
if [[ ! -e "$tmp/x.yaml" ]] && ! compgen -G "$tmp/x.yaml.tmp.*" >/dev/null; then
  ok "a rejected run leaves no output and no temp file"
else
  bad "a rejected run left a file behind"
  ls -1 "$tmp" >"$tmp/listing" || true
  indent "$tmp/listing"
fi

echo "== --help is complete"

# A hardcoded `sed -n '4,32p'` over the script's own header printed the options
# plus half of the paragraph below them, so --help ended mid-clause — and it
# drifted every time the header gained a line. Assert the shape instead of the
# line count: the usage marker must be present, the terminator must not leak,
# and the last line must be a whole line rather than a severed clause.
"$GEN" --help >"$tmp/help" 2>&1
help_ok=1
grep -q '^Usage:' "$tmp/help" || { bad "--help does not start with Usage:"; help_ok=0; }
grep -q '^  -n <count>' "$tmp/help" || { bad "--help omits the -n option"; help_ok=0; }
grep -q -- 'end usage' "$tmp/help" && { bad "--help leaks its own end-of-usage marker"; help_ok=0; }
# The old bug's signature: the final line ended with a dangling ", a" / "; a".
if tail -1 "$tmp/help" | grep -qE '[,;] *[a-z]$'; then
  bad "--help ends mid-clause (line-range drift)"
  indent "$tmp/help" 40
  help_ok=0
fi
(( help_ok )) && ok "--help prints a complete usage block"

echo "== the output is valid YAML with the expected broker count"

# Optional: only pyyaml can answer "is this actually parseable", and the gate
# must not depend on it being installed. Skipping is reported, not silent — a
# check that quietly inspects nothing is not a check.
if python3 -c 'import yaml' >/dev/null 2>&1; then
  if python3 - "$tmp/gen200.yaml" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
brokers = doc["brokers"]
assert len(brokers) == 200, f"{len(brokers)} brokers, want 200"
assert brokers["broker-200"]["url"] == "http://${MOCK_HOST}:18280", brokers["broker-200"]["url"]
assert doc["semp"]["request_min_interval"] == "0s", doc["semp"]["request_min_interval"]
PY
  then
    ok "N=200 parses as YAML with 200 brokers and the pacer off"
  else
    bad "N=200 is not valid YAML, or its contents are wrong"
  fi
else
  echo "  skip  YAML parse check (python3 yaml module not installed)"
fi

echo
echo "$pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
