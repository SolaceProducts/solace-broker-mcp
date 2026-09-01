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
# used by test-negative-paths.sh (SOL-150767) and a tight retry budget.
# Kept local to this suite so other suites' server configs stay minimal.
#
# broker-bad-creds points at a live broker with a literal wrong password so
# an auth failure surfaces through the tool path; broker-dead points at a
# closed port so connection failures surface as a retries-exhausted error.
# The password is a literal (not a ${VAR}) so the negative-path suite can
# grep the envelope for it and confirm no credential leak. Bash later-wins
# override: start-server.sh sources this file after lib.sh, so this
# definition supersedes the shared one.
#
# The semp: retry stanza compresses production defaults (10 retries with
# 3s→30s backoff) into a fail-fast test budget so the unreachable-broker
# case doesn't hang the smoke for minutes. Scoped to this suite because
# other suites may depend on the longer backoff to smooth over broker
# warm-up or transient blips. request_timeout_duration is deliberately
# left at its default: semp: is a global block that also applies to
# broker-a/broker-b, and broker-dead's 127.0.0.1:1 returns ECONNREFUSED
# immediately regardless of the timeout — the retry budget alone gives
# fail-fast, without shortening happy-path requests on slow CI.
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
    # 127.0.0.1:1 is a reserved port on loopback — guaranteed no listener
    # and fast ECONNREFUSED. Avoids the risk that a stray process on a
    # higher port (e.g. 19999) accepts the connection on a CI runner and
    # turns "unreachable" into "connected → weird response".
    url: "http://127.0.0.1:1"
    auth:
      mode: basic
      username: "\${E2E_A_USERNAME}"
      password: "\${E2E_A_PASSWORD}"

semp:
  retries: 2
  retry_min_interval: 500ms
  retry_max_interval: 1s
EOF
    log_info "Appended negative-path aliases (broker-bad-creds, broker-dead) and test retry budget to $config_file"
}

cleanup_fixtures() {
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}

# Config generator for the throttling scenario (SOL-153444, test-throttling.sh).
#
# Deliberately NOT the suite's write_config above: that one appends its own semp
# block (a retry budget for the negative-path aliases), and semp is a single
# global block, so appending a second one would leave the phase's limits at the
# mercy of YAML key ordering. This builds the base broker set with
# _lib_write_config and then owns the whole semp block itself.
#
# broker-throttle points at the tap rather than at the broker. The per-broker
# rate limiter and in-flight semaphore are keyed by broker alias, so this alias
# gets its own pair and the tap's record contains only traffic this scenario
# generated. broker-a/broker-b stay pointed straight at their brokers.
#
# retries: 0 is load-bearing, not tidiness. Retries are explicitly not paced —
# retryablehttp performs them inside the one limiter tick and the one semaphore
# slot (see the comment on Sender.Do) — so a single transient blip would inject
# arrivals at the tap that no gap assertion accounts for.
#
#   $1 config_file          path to write the generated YAML to
#   $2 request_min_interval e.g. "200ms", or "0" to disable the pacer
#   $3 max_concurrent       e.g. 2, or 10 to leave the cap provably slack
write_throttle_config() {
    local config_file="$1"
    local min_interval="$2"
    local max_concurrent="$3"

    _lib_write_config "$config_file"
    cat >> "$config_file" <<EOF
  broker-throttle:
    # Points at semp-tap, which forwards to broker-a and records what the
    # broker actually receives. Same credentials — the tap is transparent.
    url: "${SEMP_TAP_URL}"
    auth:
      mode: basic
      username: "\${E2E_A_USERNAME}"
      password: "\${E2E_A_PASSWORD}"

semp:
  request_min_interval: ${min_interval}
  max_concurrent_per_broker: ${max_concurrent}
  retries: 0
EOF
    log_info "Throttle config written to $config_file (request_min_interval=$min_interval, max_concurrent_per_broker=$max_concurrent)"
}
