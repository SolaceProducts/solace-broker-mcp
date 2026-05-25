#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> docker compose down -v (removes containers, network, and anonymous volumes)"
docker compose --project-directory "${HERE}" down -v

echo
echo "==> done"
