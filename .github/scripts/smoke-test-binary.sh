#!/usr/bin/env bash
#
# Executes a released binary archive and asserts it actually runs.
#
# WHY THIS EXISTS
#
# release.yml builds four cross-compiled binaries and, before this script,
# never ran any of them: the gate was compile-plus-attest. A binary that builds
# but fails at startup — a bad cross-compile, a missing runtime dependency, a
# packaging mistake that ships the wrong file — would reach a checksum and a
# provenance attestation with neither having actually executed the thing. See
# SOL-153189.
#
# WHAT IT CHECKS
#
#   1. The archive extracts and contains an executable named "solace-broker-mcp".
#   2. `./solace-broker-mcp --version` exits 0 — not merely that the process
#      started, an exit code.
#   3. Its stdout, trimmed, equals the expected version exactly — so a stale
#      ldflags stamp or a mis-packaged binary from a different build fails
#      loudly instead of shipping silently.
#
# Every failure names the platform (goos/goarch), since this runs once per
# matrix leg and a bare "smoke test failed" in the Actions log does not say
# which of four archives is broken.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# Anything requiring a broker connection. That needs credentials in the release
# workflow, which is exactly the secret exposure the recent `secrets: inherit`
# removal was closing — see release.yml's build-docker comment. Functional/E2E
# coverage against a real broker already lives in build-and-test.yml and the
# E2E suites.
#
# Usage: smoke-test-binary.sh <archive-path> <expected-version> <goos> <goarch>
# Exits 0 when the binary runs and reports the expected version, 1 otherwise.

set -euo pipefail

ARCHIVE="${1:?usage: smoke-test-binary.sh <archive-path> <expected-version> <goos> <goarch>}"
EXPECTED_VERSION="${2:?usage: smoke-test-binary.sh <archive-path> <expected-version> <goos> <goarch>}"
GOOS="${3:?usage: smoke-test-binary.sh <archive-path> <expected-version> <goos> <goarch>}"
GOARCH="${4:?usage: smoke-test-binary.sh <archive-path> <expected-version> <goos> <goarch>}"

BIN_NAME="solace-broker-mcp"
PLATFORM="${GOOS}/${GOARCH}"

if [ ! -f "$ARCHIVE" ]; then
    echo "::error::[$PLATFORM] archive not found: $ARCHIVE" >&2
    exit 1
fi

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# The archived artifact, not a freshly built binary — packaging bugs (wrong
# file bundled, tar built from a different GOOS/GOARCH leg) are in scope.
if ! tar xzf "$ARCHIVE" -C "$TMPDIR"; then
    echo "::error::[$PLATFORM] archive did not extract: $ARCHIVE" >&2
    exit 1
fi

BIN="$TMPDIR/$BIN_NAME"
if [ ! -f "$BIN" ]; then
    echo "::error::[$PLATFORM] $BIN_NAME not found at the archive root after extraction" >&2
    exit 1
fi

if [ ! -x "$BIN" ]; then
    echo "::error::[$PLATFORM] $BIN_NAME is not executable" >&2
    exit 1
fi

output=""
exit_code=0
output=$("$BIN" --version 2>&1) || exit_code=$?

if [ "$exit_code" -ne 0 ]; then
    echo "::error::[$PLATFORM] $BIN_NAME --version exited $exit_code, expected 0. Output:" >&2
    echo "$output" >&2
    exit 1
fi

# Trim surrounding whitespace/newlines so a trailing newline from `fmt.Println`
# doesn't fail an otherwise-exact match.
actual_version="$(printf '%s' "$output" | tr -d '[:space:]')"
expected_trimmed="$(printf '%s' "$EXPECTED_VERSION" | tr -d '[:space:]')"

if [ "$actual_version" != "$expected_trimmed" ]; then
    echo "::error::[$PLATFORM] $BIN_NAME --version reported '$actual_version', expected '$expected_trimmed'. A stale or mis-stamped binary must fail the release." >&2
    exit 1
fi

echo "✅ [$PLATFORM] $BIN_NAME --version reported '$actual_version' with a clean exit."
