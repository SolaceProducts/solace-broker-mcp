#!/usr/bin/env bash
# Basic-MCP suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, base broker fixtures, MCP wire,
# assertions, test runner) lives in the shared library. The protocol scenarios
# exercise exactly that base set, so this file adds only the two per-broker
# orchestrators below.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

create_fixtures() {
    cleanup_fixtures
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
}

# Override the shared write_config to append the two negative-path aliases
# used by test-negative-paths.sh (SOL-150767). Kept local to this suite so
# other suites' server configs stay minimal.
#
# broker-bad-creds points at a live broker with a literal wrong password so
# an auth failure surfaces through the tool path; broker-dead points at a
# closed port so connection failures surface as a retries-exhausted error.
# The password is a literal (not a ${VAR}) so the negative-path suite can
# grep the envelope for it and confirm no credential leak. Bash later-wins
# override: start-server.sh sources this file after lib.sh, so this
# definition supersedes the shared one.
write_config() {
    local config_file="$1"
    _lib_write_config "$config_file"
    cat >> "$config_file" <<EOF
  broker-bad-creds:
    url: "${BROKER_A_URL}"
    auth:
      mode: basic
      username: "\${E2E_A_USERNAME}"
      password: "wrong-password-for-e2e-negative-path"
  broker-dead:
    url: "http://localhost:19999"
    auth:
      mode: basic
      username: "\${E2E_A_USERNAME}"
      password: "\${E2E_A_PASSWORD}"
EOF
    log_info "Appended negative-path aliases (broker-bad-creds, broker-dead) to $config_file"
}

cleanup_fixtures() {
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}
