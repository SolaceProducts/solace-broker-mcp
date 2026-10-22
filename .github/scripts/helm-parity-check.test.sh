#!/usr/bin/env bash
#
# Self-test for helm-parity-check.sh.
#
# A drift check that only ever sees matching trees never proves it can fail.
# Each case copies the chart and deploy/kubernetes/ to a scratch directory,
# makes one change to one side, and asserts on the exit code: a shape where a
# correct check and a plausibly broken one (one that skips a file, compares
# only one direction, or normalises too much) would disagree. The cases that
# must pass pin the normalisation: comments, key order and Secret values are
# not drift.
#
# Usage: .github/scripts/helm-parity-check.test.sh
# Needs helm, yq (mikefarah, v4) and dyff, as helm-parity-check.sh does, and perl.

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/helm-parity-check.sh"

# edit() needs perl; without it every case would abort as if its edit had
# changed nothing.
if ! command -v perl >/dev/null; then
    echo "helm-parity-check.test: perl not found" >&2
    exit 2
fi

pass=0
fail=0

# Every fixture lives under one scratch directory that the EXIT trap removes.
# Cases call fixture through $(...), a subshell, so a list of paths it appended
# to would never reach this shell's trap.
SCRATCH=$(mktemp -d)
trap 'rm -rf "$SCRATCH"' EXIT

# A fresh copy of both install paths; prints its root. The case edits it, then
# runs the check against it with CHART_DIR and RAW_DIR.
fixture() {
    local tmp
    tmp=$(mktemp -d "$SCRATCH/case.XXXXXX")
    cp -R "$REPO_ROOT/deploy/charts/solace-broker-mcp" "$tmp/chart"
    cp -R "$REPO_ROOT/deploy/kubernetes" "$tmp/raw"
    echo "$tmp"
}

# edit <perl substitution> <file>...: edits each file in place. perl, not sed,
# because GNU and BSD sed disagree on -i and on \n in a replacement. A file the
# substitution leaves unchanged aborts the run: the case would otherwise test
# the committed tree and pass for the wrong reason. Cases anchor on keys, not
# on today's values (numbers are incremented, strings replaced), so a change
# made to both sides alike never trips this.
edit() {
    local expr=$1 f before
    shift
    for f in "$@"; do
        before=$(cat "$f")
        perl -pi -e "$expr" "$f"
        if [ "$(cat "$f")" = "$before" ]; then
            echo "self-test setup: '$expr' changed nothing in $f: the key the case edits has gone, so update the case" >&2
            exit 1
        fi
    done
}

# assert_check <description> <expected exit code> <fixture root> [<output pattern>
#     [<forbidden pattern>]]
# The pattern, when given, must appear in the check's output: a failure has to
# name what drifted, not just exit non-zero. The forbidden pattern, when given,
# must not: a report that also names what did not drift misleads.
assert_check() {
    local desc=$1 want=$2 root=$3 pattern=${4:-} forbidden=${5:-} out rc=0
    out=$(CHART_DIR="$root/chart" RAW_DIR="$root/raw" "$CHECK" 2>&1) || rc=$?
    if [ "$rc" -ne "$want" ]; then
        echo "FAIL: $desc (exit $rc, want $want)"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1))
        return
    fi
    if [ -n "$pattern" ] && ! grep -qE -- "$pattern" <<<"$out"; then
        echo "FAIL: $desc (output does not match /$pattern/)"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1))
        return
    fi
    if [ -n "$forbidden" ] && grep -qE -- "$forbidden" <<<"$out"; then
        echo "FAIL: $desc (output matches forbidden /$forbidden/)"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1))
        return
    fi
    echo "ok: $desc"
    pass=$((pass + 1))
}

# --- the tree as committed ----------------------------------------------------

root=$(fixture)
assert_check "the committed chart and raw manifests match" 0 "$root"

# --- one-sided changes: drift, in either direction ----------------------------

root=$(fixture)
edit 's/^(  replicas: )(\d+)$/$1.($2+1)/e' "$root/raw/deployment.yaml"
assert_check "a raw Deployment change alone fails, naming the field" 1 "$root" "replicas"

root=$(fixture)
edit 's/(timeoutSeconds: )(\d+)/$1.($2+1)/e' "$root/chart/templates/service.yaml"
assert_check "a chart template change alone fails" 1 "$root" "timeoutSeconds"

root=$(fixture)
edit 's/^(      retries: )(\d+)$/$1.($2+1)/e' "$root/raw/configmap.yaml"
assert_check "a raw config.yaml value change fails" 1 "$root" "retries"

root=$(fixture)
edit 's/^  BROKER_PASSWORD: /  BROKER_PASS: /' "$root/raw/secret.yaml"
assert_check "a renamed raw Secret key fails" 1 "$root" "BROKER_PASS"

root=$(fixture)
cat >"$root/raw/serviceaccount.yaml" <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: solace-broker-mcp
EOF
assert_check "a raw resource the chart does not render fails" 1 "$root" "ServiceAccount"

# Resources are matched by kind and name, so a document with neither would
# drop out of the comparison unseen; kubectl would reject it.
root=$(fixture)
printf -- '---\nreplicas: 3\n' >>"$root/raw/service.yaml"
assert_check "a raw document with no kind fails, naming the file" 1 "$root" "service.yaml: document 1: no kind"

root=$(fixture)
printf -- '---\nKind: ServiceAccount\nmetadata:\n  name: solace-broker-mcp\n' >"$root/raw/serviceaccount.yaml"
assert_check "a raw resource with a mistyped kind key fails" 1 "$root" "serviceaccount.yaml: document 0: no kind"

root=$(fixture)
printf 'replicas: 3\n' >"$root/chart/templates/zz-kindless.yaml"
assert_check "a chart document with no kind fails" 1 "$root" "the chart rendered with ci/parity-values.yaml: document [0-9]+: no kind"

root=$(fixture)
rm "$root/raw/poddisruptionbudget.yaml"
assert_check "a chart resource with no raw counterpart fails" 1 "$root" "PodDisruptionBudget"

root=$(fixture)
edit 's/^(  maxUnavailable: )(\d+)$/$1.($2+1)/e' "$root/chart/templates/poddisruptionbudget.yaml"
assert_check "a chart-only change to an optional resource fails" 1 "$root" "maxUnavailable"

root=$(fixture)
edit 's/(interval: )(\d+)s/$1.($2+1)."s"/e' "$root/raw/servicemonitor.yaml.example"
assert_check "a ServiceMonitor example change fails (examples pass)" 1 "$root" "interval"

root=$(fixture)
edit 's/(proxy-read-timeout: ")(\d+)"/$1.($2+1)."\""/e' "$root/raw/ingress.yaml.example"
assert_check "an Ingress example change fails (examples pass)" 1 "$root" "proxy-read-timeout"

root=$(fixture)
cat >"$root/raw/hpa.yaml.example" <<'EOF'
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: solace-broker-mcp
EOF
assert_check "a new raw example the chart does not render fails" 1 "$root" "HorizontalPodAutoscaler"

root=$(fixture)
edit 's/^    app.kubernetes.io\/name: solace-broker-mcp$/    app.kubernetes.io\/name: solace-broker-mcp\n    app.kubernetes.io\/part-of: solace/' "$root/raw/service.yaml"
assert_check "an extra raw label outside the allow-list fails" 1 "$root" "part-of"

root=$(fixture)
cat >"$root/raw/serviceaccount.yml" <<'EOF2'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: solace-broker-mcp
EOF2
assert_check "a raw .yml file kubectl would apply is compared too" 1 "$root" "ServiceAccount"

root=$(fixture)
printf '{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "solace-broker-mcp"}}\n' >"$root/raw/serviceaccount.json"
assert_check "a raw .json file kubectl would apply is compared too" 1 "$root" "ServiceAccount"

# The allow-list exists for labels only the chart sets. On the raw side such a
# label can only be an edit, and one that breaks the raw install: a selector
# term no raw pod carries leaves the Service with no endpoints.
root=$(fixture)
edit 's/^  selector:$/  selector:\n    app.kubernetes.io\/instance: other/' "$root/raw/service.yaml"
assert_check "an allow-listed label on the raw side fails, naming it" 1 "$root" "app.kubernetes.io/instance"

root=$(fixture)
edit 's/^    app.kubernetes.io\/name: solace-broker-mcp$/    app.kubernetes.io\/name: solace-broker-mcp\n    helm.sh\/chart: x/' "$root/raw/poddisruptionbudget.yaml"
assert_check "a Helm-only label on the raw side fails" 1 "$root" "helm.sh/chart"

# Names differ only on the renamed resource, so the report must list exactly
# that one on each side, not every kind crossed with every name.
root=$(fixture)
edit 's/^  name: solace-broker-mcp$/  name: solace-broker-mcp-pdb/' "$root/raw/poddisruptionbudget.yaml"
assert_check "a renamed raw resource is reported alone, on both sides" 1 "$root" \
    "^  PodDisruptionBudget/solace-broker-mcp-pdb$" \
    "^  (ConfigMap|Deployment|NetworkPolicy|Secret|Service)/"

for ext in yml json; do
    root=$(fixture)
    if [ "$ext" = json ]; then
        printf '{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": {"name": "solace-broker-mcp"}}\n' >"$root/raw/hpa.json.example"
    else
        printf 'apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata:\n  name: solace-broker-mcp\n' >"$root/raw/hpa.yml.example"
    fi
    assert_check "a new raw .$ext.example the chart does not render fails" 1 "$root" "HorizontalPodAutoscaler"
done

# The examples values must be the main values plus the two optional resources:
# otherwise the examples are compared against an install the raw files no
# longer describe.
root=$(fixture)
edit 's/^  tag: .*$/  tag: "self-test"/' "$root/chart/ci/parity-examples-values.yaml"
assert_check "examples values that drift from the main parity values fail" 1 "$root" "must be ci/parity-values.yaml plus"

root=$(fixture)
edit 's/^  tag: .*$/  tag: "self-test"/' "$root/chart/ci/parity-values.yaml"
assert_check "main parity values that drift from the examples values fail" 1 "$root" "must be ci/parity-values.yaml plus"

# yq reads YAML 1.2, where an unquoted off is the string "off"; kubectl and Helm
# read YAML 1.1, where it is the boolean false. Both sides can agree for yq and
# still both be wrong for the cluster.
root=$(fixture)
edit 's/proxy-buffering: "off"/proxy-buffering: off/' "$root/raw/ingress.yaml.example"
assert_check "an unquoted YAML 1.1 boolean in a raw example fails, naming it" 1 "$root" \
    "Ingress/solace-broker-mcp: metadata.annotations.nginx.ingress.kubernetes.io/proxy-buffering = off"

root=$(fixture)
edit 's/^  sessionAffinity: ClientIP$/  sessionAffinity: On/' "$root/raw/service.yaml" "$root/chart/templates/service.yaml"
assert_check "an unquoted YAML 1.1 boolean on both sides fails" 1 "$root" "spec.sessionAffinity = On"

root=$(fixture)
edit 's/^    clientIP:$/    yes:/' "$root/raw/service.yaml" "$root/chart/templates/service.yaml"
assert_check "an unquoted YAML 1.1 boolean as a key on both sides fails" 1 "$root" "sessionAffinityConfig.yes = yes"

root=$(fixture)
edit 's/^  sessionAffinity: ClientIP$/  sessionAffinity: oN/' "$root/raw/service.yaml" "$root/chart/templates/service.yaml"
assert_check "a mixed-case word YAML 1.1 does not read as a boolean is not flagged" 0 "$root"

# --- normalisation: not drift --------------------------------------------------

# The allow-list removes instance from this chart's own labels and selectors
# only. The same key elsewhere, here selecting another app's pods as the raw
# NetworkPolicy suggests, is compared like any other, so a change made to both
# sides alike is in step.
root=$(fixture)
edit 's/^(              kubernetes\.io\/metadata\.name: monitoring)$/$1\n          podSelector:\n            matchLabels:\n              app.kubernetes.io\/instance: kube-prometheus-stack-prometheus/' "$root/raw/networkpolicy.yaml"
edit 's/^(              kubernetes\.io\/metadata\.name: \{\{ \$ns \| quote \}\})$/$1\n          podSelector:\n            matchLabels:\n              app.kubernetes.io\/instance: kube-prometheus-stack-prometheus/' "$root/chart/templates/networkpolicy.yaml"
assert_check "another app's instance label outside the allow-listed paths, on both sides, is not drift" 0 "$root"

root=$(fixture)
edit 's/^(              kubernetes\.io\/metadata\.name: monitoring)$/$1\n          podSelector:\n            matchLabels:\n              app.kubernetes.io\/instance: kube-prometheus-stack-prometheus/' "$root/raw/networkpolicy.yaml"
assert_check "the same label on the raw side alone is drift" 1 "$root" "podSelector"

root=$(fixture)
edit '$. == 1 && s/^#.*$/# A reworded comment./' "$root/raw/configmap.yaml"
edit 's/^(    port: \d+)$/$1  # a new trailing comment/' "$root/raw/configmap.yaml"
assert_check "comment-only changes are not drift" 0 "$root"

root=$(fixture)
edit 's/^    log_level: (\w+)$/    log_level: "$1"/' "$root/raw/configmap.yaml"
assert_check "re-quoting a config.yaml scalar is not drift (compared parsed)" 0 "$root"

root=$(fixture)
edit 's/^(  BROKER_PASSWORD: )/$1x/' "$root/raw/secret.yaml"
assert_check "a raw Secret value change is not drift (compared by key)" 0 "$root"

# --- a broken render is a failure, never a vacuous pass ----------------------

root=$(fixture)
printf '{{ fail "boom" }}\n' >"$root/chart/templates/zz-broken.yaml"
assert_check "a chart that fails to render fails the check" 1 "$root" "boom"

root=$(fixture)
rm "$root/raw/"*.yaml
assert_check "an empty raw directory fails" 1 "$root"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
