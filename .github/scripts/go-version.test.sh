#!/usr/bin/env bash
#
# Self-test for go-version.sh. Each refused shape is one the image and the
# release binaries could otherwise split on, so each gets its own case, plus the
# committed go.mod, which must keep passing.
#
# Usage: .github/scripts/go-version.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
SCRIPT="$REPO_ROOT/.github/scripts/go-version.sh"

pass=0
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# check <description> <expected exit> <expected stdout, or "" for none> <go.mod content>
check() {
    local desc="$1" want_exit="$2" want_out="$3" content="$4"
    printf '%b' "$content" >"$tmp/go.mod"
    local got_exit=0 out
    out=$("$SCRIPT" "$tmp/go.mod" 2>/dev/null) || got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ] && [ "$out" = "$want_out" ]; then
        echo "  ok       $desc"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (exit $got_exit, stdout '$out'; want exit $want_exit, stdout '$want_out')"
        fail=$((fail + 1))
    fi
}

echo "go-version.sh self-test"

committed=$("$SCRIPT" "$REPO_ROOT/go.mod") || committed=""
if [[ "$committed" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "  ok       the committed go.mod passes ($committed)"
    pass=$((pass + 1))
else
    echo "  NOT OK   the committed go.mod does not pass"
    fail=$((fail + 1))
fi

check "a full X.Y.Z go line prints it" 0 "1.26.10" \
    'module example.com/m\n\ngo 1.26.10\n'
check "a two-part go line is refused" 1 "" \
    'module example.com/m\n\ngo 1.26\n'
check "a toolchain line is refused" 1 "" \
    'module example.com/m\n\ngo 1.26.10\n\ntoolchain go1.26.11\n'
check "a missing go line is refused" 1 "" \
    'module example.com/m\n'
check "CRLF line endings are refused" 1 "" \
    'module example.com/m\r\n\r\ngo 1.26.10\r\n'
# A `toolchain` word inside a require block is a module path, not a directive.
check "a module named toolchain is not a toolchain line" 0 "1.26.10" \
    'module example.com/m\n\ngo 1.26.10\n\nrequire (\n\texample.com/toolchain v1.0.0\n)\n'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
