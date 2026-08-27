#!/usr/bin/env bash
#
# Self-test for sbom-check.sh.
#
# One case runs the real pipeline end to end (generate a real SBOM, check it
# against the real committed THIRD_PARTY_LICENSES.md) so this proves the whole
# thing genuinely works today, not just against synthetic fixtures. The rest
# are small, crafted SBOM/document pairs isolating one property each — in
# particular the module-vs-package granularity case, which is the one this
# script exists to get right and the one a naive string-set comparison would
# get wrong on every single run.
#
# Usage: .github/scripts/sbom-check.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/sbom-check.sh"
DOC="THIRD_PARTY_LICENSES.md"

pass=0
fail=0

# Every temp dir this file creates is tracked here and removed once at exit,
# rather than an explicit rm -rf per case that a mid-case `set -e` abort would
# skip — same pattern smoke-test-binary.test.sh already uses for this file's
# sibling.
ALL_TMP_DIRS=()
cleanup() { [ "${#ALL_TMP_DIRS[@]}" -eq 0 ] || rm -rf "${ALL_TMP_DIRS[@]}"; }
trap cleanup EXIT

# assert_check <description> <expected exit code> <sbom-json> <doc-content>
assert_check() {
    local desc="$1" want="$2" sbom_json="$3" doc_content="$4"
    local tmp got=0

    tmp=$(mktemp -d)
    ALL_TMP_DIRS+=("$tmp")
    printf '%s' "$sbom_json" >"$tmp/sbom.json"
    printf '%s' "$doc_content" >"$tmp/$DOC"

    (cd "$tmp" && "$CHECK" sbom.json >/dev/null 2>&1) || got=$?

    if [ "$got" -eq "$want" ]; then
        echo "  ok       $desc (exit $got)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        fail=$((fail + 1))
    fi
}

# A minimal, valid document table wrapping the given rows — just enough for
# sbom-check.sh's own extraction regex, not a realistic full document.
doc_with_rows() { # <rows...>
    printf '| Component | Version | License | Link |\n|---|---|---|---|\n'
    printf '%s\n' "$@"
}

sbom_with_components() { # <name1> <version1> [<name2> <version2> ...]
    local components="[]"
    while [ "$#" -ge 2 ]; do
        components=$(jq --arg n "$1" --arg v "$2" '. + [{"name": $n, "version": $v}]' <<<"$components")
        shift 2
    done
    jq -n --argjson c "$components" '{"bomFormat": "CycloneDX", "specVersion": "1.6", "components": $c}'
}

echo "sbom-check.sh self-test"
echo "-- real pipeline, end to end --"

real_sbom_dir=$(mktemp -d)
ALL_TMP_DIRS+=("$real_sbom_dir")
real_sbom="$real_sbom_dir/sbom.json"
# `go run ...@v1.10.0`, matching release.yml's real "Generate SBOM" step
# exactly — that step never `go install`s the tool, so a pre-installed
# $GOBIN/cyclonedx-gomod would pass here and not exist in real CI.
#
# A failure to even generate the SBOM counts as a failed case, not a skip —
# a self-test that quietly skips its own most important case on tool trouble
# is a vacuous pass waiting to happen, the exact failure mode every other
# self-test in this directory is written to avoid.
gen_output=""
gen_rc=0
gen_output=$(go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0 \
    app -json -licenses -main cmd/server -output "$real_sbom" "$REPO_ROOT" 2>&1) || gen_rc=$?
if [ "$gen_rc" -ne 0 ]; then
    echo "  NOT OK   could not generate a real SBOM to test against:"
    while IFS= read -r line; do echo "           $line"; done <<<"$gen_output"
    fail=$((fail + 1))
else
    got=0
    (cd "$REPO_ROOT" && "$CHECK" "$real_sbom" >/dev/null 2>&1) || got=$?
    if [ "$got" -eq 0 ]; then
        echo "  ok       a freshly generated real SBOM matches the committed THIRD_PARTY_LICENSES.md (exit 0)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   a freshly generated real SBOM should match the committed document (expected exit 0, got $got)"
        fail=$((fail + 1))
    fi
fi

echo "-- synthetic fixtures, one property each --"

assert_check "a matching SBOM and document pass" 0 \
    "$(sbom_with_components "github.com/example/foo" "v1.0.0")" \
    "$(doc_with_rows '| `github.com/example/foo` | v1.0.0 | MIT | [license](x) |')"

assert_check "an SBOM component missing from the document fails" 1 \
    "$(sbom_with_components "github.com/example/foo" "v1.0.0" "github.com/example/bar" "v2.0.0")" \
    "$(doc_with_rows '| `github.com/example/foo` | v1.0.0 | MIT | [license](x) |')"

assert_check "a documented component missing from the SBOM fails" 1 \
    "$(sbom_with_components "github.com/example/foo" "v1.0.0")" \
    "$(doc_with_rows \
        '| `github.com/example/foo` | v1.0.0 | MIT | [license](x) |' \
        '| `github.com/example/bar` | v2.0.0 | MIT | [license](x) |')"

assert_check "a version mismatch between SBOM and document fails" 1 \
    "$(sbom_with_components "github.com/example/foo" "v1.0.0")" \
    "$(doc_with_rows '| `github.com/example/foo` | v0.9.0 | MIT | [license](x) |')"

# The property this script exists for: a document row one package deeper than
# its module (the go-jose/json, x/sys/cpu shape) must resolve to the SBOM's
# module-level entry, not be flagged as missing.
assert_check "a document row deeper than its module still resolves to the SBOM's module entry" 0 \
    "$(sbom_with_components "golang.org/x/example" "v1.0.0")" \
    "$(doc_with_rows '| `golang.org/x/example/subpkg` | v1.0.0 | MIT | [license](x) |')"

# The go-jose shape specifically: two document rows for the same module (the
# module itself, plus a sub-package whose licence differs and therefore keeps
# its own row) must both resolve cleanly against the single SBOM entry.
assert_check "two document rows for one module (a distinct-licence sub-package) both resolve" 0 \
    "$(sbom_with_components "github.com/example/parent" "v1.0.0")" \
    "$(doc_with_rows \
        '| `github.com/example/parent` | v1.0.0 | Apache-2.0 | [license](x) |' \
        '| `github.com/example/parent/sub` | v1.0.0 | BSD-3-Clause | [license](x) |')"

# A commit-pinned module: the SBOM (via `go list`) reports the full pseudo-
# version, the document deliberately records the bare commit hash. Must not be
# flagged as a version mismatch.
assert_check "a commit-pinned module's pseudo-version matches its documented bare commit hash" 0 \
    "$(sbom_with_components "github.com/example/pinned" "v0.0.0-20180127040702-4e3ac2762d5f")" \
    "$(doc_with_rows '| `github.com/example/pinned` | 4e3ac2762d5f | MIT | [license](x) |')"

assert_check "an empty SBOM component list fails rather than vacuously passing" 1 \
    "$(sbom_with_components)" \
    "$(doc_with_rows '| `github.com/example/foo` | v1.0.0 | MIT | [license](x) |')"

# SBOM content that isn't valid JSON at all is a jq *parse* error, distinct
# from the missing-`components`-key case above that `.components[]?` already
# handles — this one needs its own explicit catch.
assert_check "an SBOM that isn't valid JSON fails with a clean error, not a raw jq trace" 1 \
    "not json at all" \
    "$(doc_with_rows '| `github.com/example/foo` | v1.0.0 | MIT | [license](x) |')"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
