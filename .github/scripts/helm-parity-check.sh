#!/usr/bin/env bash
#
# Fails when the Helm chart and the raw manifests in deploy/kubernetes/ stop
# describing the same install.
#
# WHY THIS EXISTS
#
# The repo ships two install paths: the chart (deploy/charts/solace-broker-mcp)
# and the hand-written manifests in deploy/kubernetes/, which keep their
# explanatory comments. Nothing else ties them together, so a change to one
# would silently leave the other behind (SOL-156330). Generating the raw files
# from the chart was rejected: their comments would have to move into
# templates, and the output would carry Helm's headers and labels.
#
# HOW IT WORKS: TWO PASSES
#
#   1. Main pass. The chart rendered with ci/parity-values.yaml (the values
#      that reproduce `kubectl apply -f deploy/kubernetes/`) against every
#      file that apply reads: deploy/kubernetes/*.json, *.yaml and *.yml.
#   2. Examples pass. The chart rendered with ci/parity-examples-values.yaml,
#      which also turns on the ServiceMonitor and Ingress, against every
#      deploy/kubernetes/*.{json,yaml,yml}.example. Only the kinds this pass
#      adds to the main pass are compared: turning the ServiceMonitor on also sets
#      OBS_METRICS_SCRAPE_ENABLED on the Deployment, which the raw Deployment
#      carries only as a commented entry.
#
# Resources are matched by kind and name, in both directions: one present on
# only one side fails. Everything else is compared structurally with dyff after
# the normalisation below, applied to both sides alike. A raw file carrying a
# key the normalisation removes fails on its own (raw_guard), and so does either
# side carrying an unquoted word YAML 1.1 reads as a boolean (yaml11_guard), or a
# document with no kind or name, which would otherwise drop out (shape_guard).
#
# WHAT IT CANNOT SEE
#
# YAML comments, so the raw files' commented-out alternatives (oauth, the
# OBS_* env entries, the observability: block) are not compared. The chart's
# helm-unittest suites cover those instead (tests/oauth_test.yaml,
# tests/observability_test.yaml, tests/observability_tunables_test.yaml).
#
# Usage: .github/scripts/helm-parity-check.sh
#   CHART_DIR and RAW_DIR override the two directories (the self-test uses
#   them). Needs helm, yq (mikefarah, v4) and dyff.
#
# Exit codes: 0 in step, 1 drift (or a render that fails), 2 a missing tool.

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHART_DIR=${CHART_DIR:-$REPO_ROOT/deploy/charts/solace-broker-mcp}
RAW_DIR=${RAW_DIR:-$REPO_ROOT/deploy/kubernetes}

for tool in helm yq dyff; do
    if ! command -v "$tool" >/dev/null; then
        echo "helm-parity-check: $tool not found (prerequisites: deploy/charts/solace-broker-mcp/README.md)" >&2
        exit 2
    fi
done
# The Python yq of the same name takes different arguments entirely.
if ! yq --version 2>&1 | grep -q mikefarah; then
    echo "helm-parity-check: yq is not mikefarah/yq v4 ($(yq --version 2>&1 | head -1))" >&2
    exit 2
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# THE NORMALISATION ALLOW-LIST. Short and fixed on purpose: every entry is an
# intended difference between the two install paths, not drift, and adding one
# is a reviewed change. Applied to every document on both sides.
NORMALISE='
  # 1. Labels only Helm sets, on the resource and on the pod template.
  del(.metadata.labels."helm.sh/chart") |
  del(.metadata.labels."app.kubernetes.io/managed-by") |
  del(.metadata.labels."app.kubernetes.io/version") |
  del(.spec.template.metadata.labels."helm.sh/chart") |
  del(.spec.template.metadata.labels."app.kubernetes.io/managed-by") |
  del(.spec.template.metadata.labels."app.kubernetes.io/version") |

  # 2. app.kubernetes.io/instance, from labels and from every selector. The
  #    chart selects on name + instance so two releases can share a namespace
  #    (SOL-155108); the raw manifests select on name only.
  del(.metadata.labels."app.kubernetes.io/instance") |
  del(.spec.template.metadata.labels."app.kubernetes.io/instance") |
  del(.spec.selector.matchLabels."app.kubernetes.io/instance") |
  del(.spec.selector."app.kubernetes.io/instance") |
  del(.spec.podSelector.matchLabels."app.kubernetes.io/instance") |
  del(.spec.template.spec.topologySpreadConstraints[].labelSelector.matchLabels."app.kubernetes.io/instance") |

  # 3. The pod template checksum annotations, which roll pods on helm upgrade.
  del(.spec.template.metadata.annotations."checksum/config") |
  del(.spec.template.metadata.annotations."checksum/secret") |
  with(select(.spec.template.metadata.annotations | (tag == "!!map" and length == 0)); del(.spec.template.metadata.annotations)) |

  # 4. The ConfigMap embedded config.yaml, compared as parsed YAML, not text.
  with(select(.kind == "ConfigMap"); .data."config.yaml" |= from_yaml) |

  # 5. The Secret by key names, not values: both sides carry placeholders.
  with(select(.kind == "Secret"); .stringData[] = "") |

  # Comments are documentation, not configuration.
  ... comments = ""
'

# render <values file> <out>: the chart, rendered. A failed render is a failure,
# never an empty side that compares equal to nothing.
render() {
    if ! helm template solace-broker-mcp "$CHART_DIR" -f "$CHART_DIR/ci/$1" >"$2" 2>"$WORK/helm.err"; then
        echo "helm-parity-check: the chart failed to render with ci/$1:" >&2
        cat "$WORK/helm.err" >&2
        exit 1
    fi
}

# normalise <in> <out> [<kinds regex>]: apply the allow-list, keep only the
# documents whose kind matches (default all), and sort them by kind and name
# so the file order on either side does not matter.
normalise() {
    local kinds=${3:-.*}
    KINDS="^($kinds)\$" yq eval-all "
        [. | select(kind == \"map\") | select(.kind | test(strenv(KINDS)))] |
        sort_by(.kind, .metadata.name) | .[] | ($NORMALISE) | split_doc
    " "$1" >"$2"
}

# ids <file>: one "Kind/name" line per document. eval, not eval-all: across
# all documents at once, + would pair every kind with every name.
ids() {
    yq eval -N 'select(kind == "map") | .kind + "/" + .metadata.name' "$1" | sort
}

# concat <out> <file>...: the files as one multi-document stream. Each raw
# file starts and may end with a comment, so the separators are explicit.
concat() {
    local out=$1 f
    shift
    : >"$out"
    for f in "$@"; do
        { cat "$f"; echo; echo "---"; } >>"$out"
    done
}

# raw_guard <raw stream> <label>: returns 1 when a raw document carries a key the
# allow-list removes, at a path where it removes it. The allow-list removes
# labels and annotations only the chart sets, from both sides alike. On the raw
# side such a key can only be an edit, and one the comparison would hide: a
# selector term no raw pod carries leaves the Service with no endpoints. The
# same key anywhere else (another app's pods selected by instance) is compared
# like any other. The paths and keys here are the ones NORMALISE deletes; keep
# the two in step.
LABEL_KEYS='^(helm\.sh/chart|app\.kubernetes\.io/(managed-by|version|instance))$'
SELECTOR_KEYS='^app\.kubernetes\.io/instance$'
ANNOTATION_KEYS='^checksum/(config|secret)$'
raw_guard() {
    local found
    found=$(LABELS="$LABEL_KEYS" SELECTORS="$SELECTOR_KEYS" ANNOTATIONS="$ANNOTATION_KEYS" yq eval -N '
        select(kind == "map") | (.kind + "/" + .metadata.name) as $id |
        [
          ((.metadata.labels, .spec.template.metadata.labels) |
            select(kind == "map") | keys | .[] | select(test(strenv(LABELS)))),
          (.spec.template.metadata.annotations |
            select(kind == "map") | keys | .[] | select(test(strenv(ANNOTATIONS)))),
          ((.spec.selector, .spec.selector.matchLabels, .spec.podSelector.matchLabels,
            (.spec.template.spec.topologySpreadConstraints // [] | .[] | .labelSelector.matchLabels)) |
            select(kind == "map") | keys | .[] | select(test(strenv(SELECTORS))))
        ] | unique | .[] | $id + ": " + .
    ' "$1") || { echo "helm-parity-check: yq failed on $1" >&2; exit 1; }
    if [ -n "$found" ]; then
        echo "in $2, keys only the chart may set (normalised away, so a raw edit to them would go unseen):"
        echo "$found" | sed 's/^/  /'
        return 1
    fi
}

# shape_guard <label> <file>...: returns 1 when a non-empty document is not a
# mapping with a string kind and metadata.name. Resources are matched by those
# two, so such a document would drop out of the comparison unseen, while
# kubectl or Helm would reject it. Checked per file so the report names it.
SHAPE='
  select(tag != "!!null") | ("document " + (document_index | tostring) + ": ") as $at |
  (if kind != "map" then $at + "not a mapping"
   elif (.kind | tag) != "!!str" or .kind == "" then $at + "no kind"
   elif (.metadata | kind) != "map" then $at + .kind + " with no metadata.name"
   elif (.metadata.name | tag) != "!!str" or .metadata.name == "" then $at + .kind + " with no metadata.name"
   else null end) | select(. != null)
'
shape_guard() {
    local label=$1 f out name found=""
    shift
    for f in "$@"; do
        out=$(yq eval -N "$SHAPE" "$f") || { echo "helm-parity-check: yq failed on $f" >&2; exit 1; }
        name=${f#"$RAW_DIR"/}
        [ "$name" != "$f" ] || name=$label
        [ -z "$out" ] || found+=$(sed "s|^|$name: |" <<<"$out")$'\n'
    done
    if [ -n "$found" ]; then
        echo "in $label, documents that are not a resource with a kind and a name:"
        printf '%s' "$found" | sed 's/^/  /'
        return 1
    fi
}

# yaml11_guard <stream> <label>: returns 1 when a plain (unquoted) scalar, key or
# value, is one of the words YAML 1.1 reads as a boolean. yq reads YAML 1.2,
# where `off` is the string "off", so both sides can match here; kubectl and
# Helm read YAML 1.1, where it is false, and an annotation that is a boolean is
# rejected. A block scalar such as the ConfigMap's config.yaml is not plain, so
# its contents are not checked: the server decodes them into typed fields, and
# its YAML library (yaml.v3) reads on/off/yes/no into a bool field as YAML 1.1
# would, so only a string field could differ.
YAML11_BOOLS='^(y|Y|yes|Yes|YES|n|N|no|No|NO|on|On|ON|off|Off|OFF)$'
yaml11_guard() {
    local found
    found=$(WORDS="$YAML11_BOOLS" yq eval -N '
        select(kind == "map") | (.kind + "/" + .metadata.name) as $id |
        ... | select(tag == "!!str" and style == "") | select(test(strenv(WORDS))) |
        $id + ": " + (path | map(tostring) | join(".")) + " = " + .
    ' "$1") || { echo "helm-parity-check: yq failed on $1" >&2; exit 1; }
    if [ -n "$found" ]; then
        echo "in $2, unquoted words kubectl and Helm read as booleans (quote them):"
        echo "$found" | sed 's/^/  /'
        return 1
    fi
}

# compare <pass> <chart file> <raw file> <raw label> <raw stream, unnormalised>
#     <chart stream for yaml11_guard> <chart label> <chart render, whole>
#     <raw file>...
drift=0
compare() {
    local pass=$1 chart=$2 raw=$3 label=$4 only pass_drift=0
    echo "== $pass"
    # The whole render, so not the kinds suffix the examples label carries.
    shape_guard "${7% (*}" "$8" || pass_drift=1
    if [ "$#" -gt 8 ]; then
        shape_guard "$label" "${@:9}" || pass_drift=1
    fi
    raw_guard "$5" "$label" || pass_drift=1
    yaml11_guard "$5" "$label" || pass_drift=1
    yaml11_guard "$6" "$7" || pass_drift=1
    ids "$chart" >"$WORK/chart.ids"
    ids "$raw" >"$WORK/raw.ids"
    if [ ! -s "$WORK/raw.ids" ]; then
        echo "no resources found in $label" >&2
        drift=1
        return
    fi
    only=$(comm -23 "$WORK/chart.ids" "$WORK/raw.ids")
    if [ -n "$only" ]; then
        echo "rendered by the chart but not in $label:"
        echo "$only" | sed 's/^/  /'
        pass_drift=1
    fi
    only=$(comm -13 "$WORK/chart.ids" "$WORK/raw.ids")
    if [ -n "$only" ]; then
        echo "in $label but not rendered by the chart:"
        echo "$only" | sed 's/^/  /'
        pass_drift=1
    fi
    # dyff reports per resource, matched by kind and name, so it still names
    # the field that differs in the resources both sides share.
    local rc=0
    dyff between --omit-header --set-exit-code --color off \
        "$raw" "$chart" || rc=$?
    case $rc in
        0) ;;
        1) pass_drift=1 ;;
        *) echo "helm-parity-check: dyff failed (exit $rc)" >&2; exit 1 ;;
    esac
    if [ "$pass_drift" -eq 0 ]; then echo "in step"; else drift=1; fi
}

# The main pass takes every file `kubectl apply -f deploy/kubernetes/` reads:
# .json, .yaml and .yml. The .example suffix keeps the examples out of that;
# the examples pass reads the same three forms with it.
shopt -s nullglob
raw_main=("$RAW_DIR"/*.json "$RAW_DIR"/*.yaml "$RAW_DIR"/*.yml)
raw_examples=("$RAW_DIR"/*.json.example "$RAW_DIR"/*.yaml.example "$RAW_DIR"/*.yml.example)
shopt -u nullglob

# Main pass.
render parity-values.yaml "$WORK/main.rendered"
normalise "$WORK/main.rendered" "$WORK/main.chart"
concat "$WORK/main.raw.in" ${raw_main[@]+"${raw_main[@]}"}
normalise "$WORK/main.raw.in" "$WORK/main.raw"
compare "main pass: ci/parity-values.yaml vs deploy/kubernetes/*.{json,yaml,yml}" \
    "$WORK/main.chart" "$WORK/main.raw" "deploy/kubernetes/*.{json,yaml,yml}" "$WORK/main.raw.in" \
    "$WORK/main.rendered" "the chart rendered with ci/parity-values.yaml" "$WORK/main.rendered" \
    ${raw_main[@]+"${raw_main[@]}"}

# Examples pass: only the kinds the examples values add to the main pass.
#
# Its values must be the main values plus the two optional resources, or the
# examples would be compared against an install the raw files no longer
# describe: a resource renamed in parity-values.yaml alone would leave the
# Ingress example routing to a Service the raw apply no longer creates.
yq '(del(.observability.serviceMonitor, .ingress) |
     with(select(.observability | length == 0); del(.observability))) |
     ... comments = ""' "$CHART_DIR/ci/parity-examples-values.yaml" >"$WORK/examples.base.yaml"
yq '... comments = ""' "$CHART_DIR/ci/parity-values.yaml" >"$WORK/main.values.yaml"
values_rc=0
dyff between --omit-header --set-exit-code --color off \
    "$WORK/main.values.yaml" "$WORK/examples.base.yaml" >"$WORK/values.diff" || values_rc=$?
case $values_rc in
    0) ;;
    1)
        echo "== ci/parity-examples-values.yaml must be ci/parity-values.yaml plus observability.serviceMonitor and ingress:"
        cat "$WORK/values.diff"
        drift=1
        ;;
    *) echo "helm-parity-check: dyff failed (exit $values_rc)" >&2; exit 1 ;;
esac
render parity-examples-values.yaml "$WORK/examples.rendered"
main_kinds=$(yq eval-all -N 'select(kind == "map") | .kind' "$WORK/main.rendered" | sort -u | paste -sd'|')
added_kinds=$(yq eval-all -N 'select(kind == "map") | .kind' "$WORK/examples.rendered" | sort -u |
    grep -vxE "$main_kinds" | paste -sd'|' || true)
if [ -z "$added_kinds" ]; then
    echo "helm-parity-check: ci/parity-examples-values.yaml renders no kind beyond the main pass" >&2
    exit 1
fi
normalise "$WORK/examples.rendered" "$WORK/examples.chart" "$added_kinds"
concat "$WORK/examples.raw.in" ${raw_examples[@]+"${raw_examples[@]}"}
normalise "$WORK/examples.raw.in" "$WORK/examples.raw"
compare "examples pass: ci/parity-examples-values.yaml ($added_kinds) vs deploy/kubernetes/*.{json,yaml,yml}.example" \
    "$WORK/examples.chart" "$WORK/examples.raw" "deploy/kubernetes/*.{json,yaml,yml}.example" "$WORK/examples.raw.in" \
    "$WORK/examples.chart" "the chart rendered with ci/parity-examples-values.yaml ($added_kinds)" \
    "$WORK/examples.rendered" ${raw_examples[@]+"${raw_examples[@]}"}

if [ "$drift" -ne 0 ]; then
    echo
    echo "The chart and deploy/kubernetes/ have drifted: change the other side to match."
    echo "Intended differences belong in the normalisation allow-list in $0."
    exit 1
fi
echo
echo "The chart and deploy/kubernetes/ are in step."
