#!/usr/bin/env bash
# Takes the real, committed deploy/otel-collector/docker/otelcol.yaml — the
# file an operator actually copies — and uncomments exactly the
# otlphttp/prometheus exporter block and the metrics: pipeline that
# reference it, leaving every other line (including the prose comments
# explaining both) untouched. Writes the result to $OUT (default
# bin/otelcol-uncommented.yaml).
#
# This is real string substitution against known, exact lines, not
# indentation math: the source comment block's internal spacing was hand
# -authored for readability as a comment, not as "correct YAML indentation
# with a # inserted", so naively stripping "# " does not reliably reproduce
# valid YAML. Each substitution asserts it matched exactly once, so a future
# reword of the source file fails this script loudly instead of silently
# leaving the pipeline commented out.
set -euo pipefail

SRC="${SRC:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../deploy/otel-collector/docker" && pwd)/otelcol.yaml}"
OUT="${OUT:-bin/otelcol-uncommented.yaml}"

mkdir -p "$(dirname "$OUT")"

python3 - "$SRC" "$OUT" <<'PYEOF'
import sys

src_path, out_path = sys.argv[1], sys.argv[2]
text = open(src_path).read()

# (exact commented line, exact uncommented replacement). Order doesn't
# matter; each must match exactly once in the source.
substitutions = [
    ("  # otlphttp/prometheus:\n", "  otlphttp/prometheus:\n"),
    ("  #   endpoint: ${env:PROMETHEUS_OTLP_ENDPOINT}\n", "    endpoint: ${env:PROMETHEUS_OTLP_ENDPOINT}\n"),
    ("  #   tls:\n", "    tls:\n"),
    ("  #     insecure: true\n", "      insecure: true\n"),
    ("    # metrics:\n", "    metrics:\n"),
    ("    #   receivers: [otlp]\n", "      receivers: [otlp]\n"),
    ("    #   processors: [batch]\n", "      processors: [batch]\n"),
    ("    #   exporters: [otlphttp/prometheus]\n", "      exporters: [otlphttp/prometheus]\n"),
]

for old, new in substitutions:
    count = text.count(old)
    if count != 1:
        sys.exit(
            f"uncomment-metrics-pipeline: expected exactly one occurrence of "
            f"{old!r} in {src_path}, found {count}. The committed file's "
            f"comment block has changed — update this script's substitution "
            f"list to match, rather than letting it silently no-op."
        )
    text = text.replace(old, new, 1)

open(out_path, "w").write(text)
print(f"wrote {out_path}")
PYEOF
