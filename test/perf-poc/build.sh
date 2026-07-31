#!/usr/bin/env bash
# Build all perf-poc binaries into test/perf-poc/bin/.
# See docs/plans/2026-07-22-semp-mock-perf-poc.md.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
outdir="$here/bin"
mkdir -p "$outdir"

cmds=(mock-semp loadgen fidelity memsampler)

for cmd in "${cmds[@]}"; do
  echo "building $cmd"
  ( cd "$here/$cmd" && go build -o "$outdir/$cmd" . )
done

echo
echo "built into $outdir:"
ls -1 "$outdir"
