#!/usr/bin/env bash
# Build all performance binaries into test/performance/bin/.
#
# Includes the MCP server itself. The run scripts exec that binary directly
# rather than `go run ./cmd/server`, because `go run` executes the compiled
# program as a *child* process: the PID the script captures is the toolchain
# wrapper, not the server, so memsampler would sample a flat ~30 MB build
# driver and report a meaningless "RSS drift PASS". Building once here also
# keeps compile time out of the measured startup path.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
outdir="$here/bin"
mkdir -p "$outdir"

cmds=(mock-semp loadgen fidelity memsampler)

for cmd in "${cmds[@]}"; do
  echo "building $cmd"
  ( cd "$here/$cmd" && go build -o "$outdir/$cmd" . )
done

echo "building mcp-server"
( cd "$repo_root" && go build -o "$outdir/mcp-server" ./cmd/server )

echo
echo "built into $outdir:"
ls -1 "$outdir"
