#!/usr/bin/env bash
# Build all performance binaries into test/performance/bin/.

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
