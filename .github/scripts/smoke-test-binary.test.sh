#!/usr/bin/env bash
#
# Self-test for smoke-test-binary.sh.
#
# A release gate that only ever sees good archives never proves it can fail.
# Every case below builds a fixture archive from scratch (a tiny shell script
# standing in for the real Go binary — smoke-test-binary.sh only cares that
# something executable emits a version string, not what runs it) and asserts
# on the resulting exit code, so each case is a shape where a correct
# implementation and a plausibly broken one would disagree.
#
# Usage: .github/scripts/smoke-test-binary.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/smoke-test-binary.sh"
BIN_NAME="solace-broker-mcp"
VERSION="v9.9.9"

pass=0
fail=0

# --- fixture builders --------------------------------------------------------
# Each returns the path to a built .tar.gz in a fresh temp dir. `set -e` in this
# file means a builder that fails aborts the whole self-test loudly, rather
# than silently producing an empty archive that makes its case vacuous.

# A binary that prints the given version and exits 0.
build_good_archive() { # <version>
    local tmp archive
    tmp=$(mktemp -d)
    cat >"$tmp/$BIN_NAME" <<EOF
#!/usr/bin/env bash
if [ "\$1" = "--version" ]; then
    echo "$1"
    exit 0
fi
exit 1
EOF
    chmod +x "$tmp/$BIN_NAME"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "$BIN_NAME"
    echo "$archive"
}

# A binary that reports the wrong version.
build_mismatched_archive() {
    local tmp archive
    tmp=$(mktemp -d)
    cat >"$tmp/$BIN_NAME" <<'EOF'
#!/usr/bin/env bash
echo "v0.0.1-stale"
exit 0
EOF
    chmod +x "$tmp/$BIN_NAME"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "$BIN_NAME"
    echo "$archive"
}

# A binary that crashes instead of exiting cleanly.
build_crashing_archive() {
    local tmp archive
    tmp=$(mktemp -d)
    cat >"$tmp/$BIN_NAME" <<'EOF'
#!/usr/bin/env bash
echo "boom" >&2
exit 1
EOF
    chmod +x "$tmp/$BIN_NAME"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "$BIN_NAME"
    echo "$archive"
}

# A file that is present but not executable — the archive is intact, the bit is not.
build_nonexecutable_archive() {
    local tmp archive
    tmp=$(mktemp -d)
    printf '#!/usr/bin/env bash\necho "%s"\n' "$VERSION" >"$tmp/$BIN_NAME"
    chmod -x "$tmp/$BIN_NAME"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "$BIN_NAME"
    echo "$archive"
}

# The archive contains something, but not a file named solace-broker-mcp — the
# packaging-bug case the story calls out explicitly.
build_missing_binary_archive() {
    local tmp archive
    tmp=$(mktemp -d)
    echo "not a binary" >"$tmp/README.txt"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "README.txt"
    echo "$archive"
}

# The binary is nested in a subdirectory rather than at the archive root, the
# way a `tar czf archive.tar.gz dist/solace-broker-mcp` packaging mistake would
# produce.
build_nested_binary_archive() {
    local tmp archive
    tmp=$(mktemp -d)
    mkdir "$tmp/dist"
    printf '#!/usr/bin/env bash\necho "%s"\n' "$VERSION" >"$tmp/dist/$BIN_NAME"
    chmod +x "$tmp/dist/$BIN_NAME"
    archive="$tmp/archive.tar.gz"
    tar czf "$archive" -C "$tmp" "dist/$BIN_NAME"
    echo "$archive"
}

# --- harness ---------------------------------------------------------------

# assert_check <description> <expected exit code> <archive> <expected-version>
assert_check() {
    local desc="$1" want="$2" archive="$3" expected_version="$4"
    local got=0

    "$CHECK" "$archive" "$expected_version" linux amd64 >/dev/null 2>&1 || got=$?

    if [ "$got" -eq "$want" ]; then
        echo "  ok       $desc (exit $got)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        fail=$((fail + 1))
    fi
}

echo "smoke-test-binary.sh self-test"

good_archive=$(build_good_archive "$VERSION")
assert_check "a good binary reporting the expected version passes" 0 \
    "$good_archive" "$VERSION"

assert_check "a version mismatch fails" 1 \
    "$good_archive" "v1.0.0-different"

assert_check "a crashing binary (non-zero exit) fails" 1 \
    "$(build_crashing_archive)" "$VERSION"

assert_check "a non-executable file fails" 1 \
    "$(build_nonexecutable_archive)" "$VERSION"

assert_check "an archive missing the named binary fails" 1 \
    "$(build_missing_binary_archive)" "$VERSION"

assert_check "a binary nested off the archive root fails" 1 \
    "$(build_nested_binary_archive)" "$VERSION"

assert_check "a nonexistent archive path fails" 1 \
    "/nonexistent/path/archive.tar.gz" "$VERSION"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
