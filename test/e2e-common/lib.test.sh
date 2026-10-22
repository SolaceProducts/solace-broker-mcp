#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# Self-test for the session-ID handling in lib.sh's MCP handshake helpers.
#
# The dangerous failure is not a crash. A helper that merely tolerated a missing
# Mcp-Session-Id would let a stateful server that stopped issuing one pass every
# e2e suite. So the assertions here are on the two directions of the contract:
# a stateful server with no ID must FAIL with a printed message, and a stateless
# server with an ID must fail too. Mode comes from MCP_TRANSPORT_STATELESS, never
# from the response.
#
# `curl` is replaced by a shell function, so this needs no server, no broker and
# no network.
#
# Usage: test/e2e-common/lib.test.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SUITE_DIR="$here/../e2e-basic-mcp"
# shellcheck source=lib.sh
source "$here/lib.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
CALLS="$tmp/curl.calls"

pass=0
fail=0

ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

# FAKE selects what the stubbed server returns: withid, noid, or fail.
FAKE=withid
curl() {
    echo "$*" >>"$CALLS"
    case "$*" in
    *"-D -"*)
        [ "$FAKE" = fail ] && return 22
        printf 'HTTP/1.1 200 OK\r\n'
        [ "$FAKE" = withid ] && printf 'Mcp-Session-Id: abc123\r\n'
        printf 'Content-Type: text/event-stream\r\n\r\n'
        printf 'event: message\ndata: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"%s"}}\n\n' "$MCP_PROTOCOL_VERSION"
        ;;
    *)
        # mcp_request returns non-zero when the response has no data: line.
        echo 'data: {}'
        ;;
    esac
    return 0
}

# Runs mcp_initialize in a subshell with the given mode and fake server, and
# leaves its stdout, stderr and exit status in OUT, ERR and RC. Assertions run
# in this shell, never in the subshell, so a failure cannot be lost there.
run_init() {
    local mode="$1"
    : >"$CALLS"
    OUT=$( (
        MCP_TRANSPORT_STATELESS="$mode"
        FAKE="$2"
        mcp_initialize
    ) 2>"$tmp/err") && RC=0 || RC=$?
    ERR=$(<"$tmp/err")
}

echo "mcp_initialize"

run_init "" withid
if [ "$RC" -eq 0 ] && [ "$OUT" = "abc123" ]; then ok "stateful, ID issued: returns the ID"; else bad "stateful, ID issued: rc=$RC out='$OUT'"; fi
if grep -q "Mcp-Session-Id: abc123" "$CALLS"; then ok "stateful: the notification carries the session header"; else bad "stateful: notification lacks the session header"; fi

# The dangerous property: this must not pass.
run_init "" noid
if [ "$RC" -ne 0 ] && [[ "$ERR" == *"No Mcp-Session-Id"* ]]; then ok "stateful, no ID: fails and prints a message"; else bad "stateful, no ID: rc=$RC err='$ERR'"; fi

run_init false noid
if [ "$RC" -ne 0 ] && [[ "$ERR" == *"No Mcp-Session-Id"* ]]; then ok "MCP_TRANSPORT_STATELESS=false, no ID: still fails"; else bad "=false, no ID: rc=$RC err='$ERR'"; fi

run_init true noid
if [ "$RC" -eq 0 ] && [ -z "$OUT" ]; then ok "stateless, no ID: succeeds with an empty ID"; else bad "stateless, no ID: rc=$RC out='$OUT' err='$ERR'"; fi
if ! grep -qi "Mcp-Session-Id" "$CALLS"; then ok "stateless: no session header is sent, not even a blank one"; else bad "stateless: a session header was sent"; fi
if grep -q "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" "$CALLS"; then ok "stateless: the protocol version header is still sent"; else bad "stateless: protocol version header missing"; fi

run_init true withid
if [ "$RC" -ne 0 ] && [[ "$ERR" == *"must not issue one"* ]]; then ok "stateless, ID issued: fails and prints a message"; else bad "stateless, ID issued: rc=$RC err='$ERR'"; fi

for mode in "" true; do
    run_init "$mode" fail
    if [ "$RC" -ne 0 ] && [[ "$ERR" == *"initialize request"* ]]; then ok "request failure (mode='$mode'): prints a message, not a silent exit"; else bad "request failure (mode='$mode'): rc=$RC err='$ERR'"; fi
done

echo "mcp_request"

: >"$CALLS"
mcp_request "" '{}' >/dev/null
if ! grep -qi "Mcp-Session-Id" "$CALLS"; then ok "empty ID: no session header"; else bad "empty ID: a session header was sent"; fi
if grep -q "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" "$CALLS"; then ok "empty ID: protocol version header still sent"; else bad "empty ID: protocol version header missing"; fi

: >"$CALLS"
mcp_request "abc123" '{}' >/dev/null
if grep -q "Mcp-Session-Id: abc123" "$CALLS"; then ok "with an ID: header sent"; else bad "with an ID: header missing"; fi

echo "mcp_is_stateless"
for v in 1 t T true TRUE True; do
    if (MCP_TRANSPORT_STATELESS="$v"; mcp_is_stateless); then ok "'$v' is stateless"; else bad "'$v' should be stateless"; fi
done
for v in "" 0 f F false FALSE False banana " true"; do
    if (MCP_TRANSPORT_STATELESS="$v"; mcp_is_stateless); then bad "'$v' should be stateful"; else ok "'$v' is stateful"; fi
done

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
