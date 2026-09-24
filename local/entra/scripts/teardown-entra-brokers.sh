#!/usr/bin/env bash
#
# Remove only the laptop Entra lab brokers. Does not touch .local/ or .env.
# Does not remove solace-local-infra containers (solace / solace-b).

set -euo pipefail

NAMES=(mcp-entra-solace mcp-entra-solace-b mcp-entra-solace-c)

if command -v docker >/dev/null 2>&1; then
  CONTAINER_CLI=docker
elif command -v podman >/dev/null 2>&1; then
  CONTAINER_CLI=podman
else
  echo "error: neither 'docker' nor 'podman' found on PATH" >&2
  exit 1
fi

for name in "${NAMES[@]}"; do
  if "$CONTAINER_CLI" inspect "$name" >/dev/null 2>&1; then
    echo "removing $name"
    "$CONTAINER_CLI" rm -f "$name" >/dev/null
  else
    echo "$name not present"
  fi
done
