#!/usr/bin/env bash
# Monitoring-suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, base broker fixtures, MCP wire,
# assertions, test runner) lives in the shared library; this file adds only the
# monitoring-specific fixtures (F1–F7) and broker-driver orchestration.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# Exported so broker-driver (spawned as a child by create_connected_client_on
# and the F4-F7 helpers) can resolve --broker=a|b to a SMF host:port from the
# same single source of truth (.env, sourced by the shared library).
export BROKER_A_SMF_PORT BROKER_B_SMF_PORT

# ── broker-driver ────────────────────────────────────────────────────────────

build_broker_driver() {
    log_info "Building broker-driver binary (CGo: libsolclient via solace.dev/go/messaging) ..."
    mkdir -p "$BIN_DIR"
    (cd "$SUITE_DIR/broker-driver" && go build -o "$BIN_DIR/broker-driver" .)
    log_info "broker-driver binary built: $BIN_DIR/broker-driver"
}

# Path pattern for broker-driver PID files. Defined once here so the
# stop helper and any future code use the same convention.
BROKER_DRIVER_PIDFILE_GLOB="$BIN_DIR/broker-driver-f*.pid"

# Stop any long-lived broker-driver processes that fixtures F3-F7 spawn.
# Reads each PID file under bin/ and hands the PIDs to kill_gracefully, which
# signals them concurrently (TERM, then KILL after a shared 5s grace window).
# Safe to call when there are no PID files.
stop_broker_drivers() {
    local pidfiles=( $BROKER_DRIVER_PIDFILE_GLOB )
    [ -e "${pidfiles[0]}" ] || return 0

    local pids=() f
    for f in "${pidfiles[@]}"; do
        pids+=("$(<"$f")")
    done
    # Resume any SIGSTOP'd driver (the F6 slow-subscriber is deliberately
    # stopped) so the SIGTERM kill_gracefully sends is actually delivered —
    # a stopped process ignores SIGTERM and would otherwise burn the full 5s
    # grace before SIGKILL. SIGCONT is a no-op on running drivers.
    # Guard with `kill -0` (as kill_gracefully does): a driver may have exited
    # early and left a stale pidfile whose PID the OS has since recycled, so we
    # only signal PIDs that are still alive and never disturb an unrelated one.
    local pid
    for pid in "${pids[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -CONT "$pid" 2>/dev/null || true
    done
    kill_gracefully "${pids[@]}"

    rm -f $BROKER_DRIVER_PIDFILE_GLOB
    # Allow the broker to finish cleaning up stale SMF sessions before
    # subsequent SEMP config operations run. Only reached when broker-drivers
    # were actually running (the early return 0 above skips this otherwise).
    sleep 3
}

# ── Broker fixtures (F1–F7, layered on the shared base set in lib.sh) ─────────

verify_multi_vpn_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-VPN fixture visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/test-vpn" || true
}

verify_multi_queue_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-queue fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-2" || true
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-3" || true
}

# Provisions a second, non-default VPN ("test-vpn") on a broker with enabled=false.
# Intentionally no client-user / ACL / queue provisioning — this VPN exists only
# to exercise multi-VPN discovery and listing, not messaging.
create_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-VPN fixture on $label ..."
    semp_post "$semp_config" "msgVpns" \
        '{"msgVpnName":"test-vpn","enabled":false}' >/dev/null
    verify_multi_vpn_on "$broker_url" "$label"
    log_info "Multi-VPN fixture created on $label (test-vpn, enabled=false)"
}

cleanup_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-VPN fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/test-vpn"
    log_info "Multi-VPN fixture cleaned up on $label"
}

# Provisions two additional queues on the default VPN to exercise multi-queue
# discovery and bound-vs-unbound state:
#   - test-queue-2: non-exclusive, bound to the existing test-rdp
#   - test-queue-3: non-exclusive, unbound
# Depends on create_fixtures_on having run first (reuses test-rdp).
create_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-queue fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-2","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-3","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings" \
        '{"queueBindingName":"test-queue-2","postRequestTarget":"/test"}' >/dev/null

    verify_multi_queue_on "$broker_url" "$label"
    log_info "Multi-queue fixtures created on $label (test-queue-2 bound to test-rdp, test-queue-3 unbound)"
}

cleanup_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-queue fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-3"
    log_info "Multi-queue fixtures cleaned up on $label"
}

# F3 connected client — single source of truth for the fixture's identifiers,
# referenced by both create_connected_client_on and verify-fixtures.sh.
F3_CLIENT_NAME_A="e2e-monitoring-connected-a"
F3_CLIENT_NAME_B="e2e-monitoring-connected-b"
F3_SUBSCRIPTIONS="e2e-monitoring/connected/t1,e2e-monitoring/connected/t2"

# Polls for a broker-driver's self-written pidfile — the driver's readiness
# signal — up to 10s (20 * 0.5s). Returns non-zero and logs which driver failed
# and where to look if the file is still absent/empty. Shared by the F3/F4/F5
# fixture starters, which differ only in the driver description ($what).
wait_for_pidfile() {
    local pidfile="$1"
    local label="$2"
    local logfile="$3"
    local what="$4"           # driver description for the failure message
    local max_attempts=20     # 20 * 0.5s = 10s
    local attempt=0
    while [ $attempt -lt $max_attempts ] && [ ! -s "$pidfile" ]; do
        sleep 0.5
        attempt=$((attempt + 1))
    done
    if [ ! -s "$pidfile" ]; then
        log_fail "$what did not create pidfile on $label within 10s; see $logfile"
        return 1
    fi
}

# Spawn a long-lived broker-driver process that binds a persistent receiver
# to test-queue and holds direct topic subscriptions, satisfying F3. The
# process self-writes a PID file that stop_broker_drivers later reaps.
# Depends on create_fixtures_on having created test-queue first.
create_connected_client_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"     # "a" or "b" — resolves SMF host:port in broker-driver
    local client_name="$4"
    local pidfile="$BIN_DIR/broker-driver-f3-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f3-$broker_letter.log"
    log_info "Creating connected-client fixture on $label (clientName=$client_name) ..."

    # nohup + setsid so the driver survives an aborted parent shell and is in
    # its own session; the bash harness still finds it via the pidfile glob.
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" connected-client \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --queue=test-queue \
        --subscriptions="$F3_SUBSCRIPTIONS" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    # Wait for the driver to self-write its pidfile (signals readiness).
    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver" || return 1

    # Then wait until the broker actually reports the client by name.
    verify_monitor_object "$broker_url" "$label" \
        "msgVpns/$BROKER_VPN/clients/$client_name"
    log_info "Connected-client fixture created on $label (PID=$(<"$pidfile"))"
}

# stop_broker_drivers handles termination via the PID file, so this is a
# label-only nop kept for symmetry with the F1/F2 create/cleanup pairs.
cleanup_connected_client_on() {
    local label="$1"
    log_info "Connected-client cleanup on $label deferred to stop_broker_drivers"
}

# F4 sustained-traffic constants. The topic must be one of F3_SUBSCRIPTIONS
# so the F3 direct receiver drains the persistent publish — that's how
# AC 5's txMsgRate threshold becomes reachable.
F4_TOPIC="e2e-monitoring/connected/t1"
F4_RATE=100      # msg/s, matches the ticket-spec target
F4_SIZE=256      # bytes per message

# Spawn a long-lived broker-driver publisher that hits F4_RATE messages per
# second on F4_TOPIC. The F3 connected-client receiver subscribes to that
# topic, so the broker observes both rxMsgRate (from publisher) and
# txMsgRate (delivered to F3 receiver).
create_sustained_traffic_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"
    local pidfile="$BIN_DIR/broker-driver-f4-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f4-$broker_letter.log"
    log_info "Creating sustained-traffic fixture on $label (rate=$F4_RATE/s topic=$F4_TOPIC) ..."

    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" publisher \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --topic="$F4_TOPIC" \
        --rate="$F4_RATE" \
        --size="$F4_SIZE" \
        --message-type=persistent \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver publisher" || return 1
    log_info "Sustained-traffic fixture started on $label (PID=$(<"$pidfile"))"
}

cleanup_sustained_traffic_on() {
    local label="$1"
    log_info "Sustained-traffic cleanup on $label deferred to stop_broker_drivers"
}

# F5 slow-consumer constants. A dedicated queue subscribes to F5_TOPIC; the
# broker-driver slow-consumer process publishes into that topic fast while a
# queue-bound receiver ACKs only every F5_ACK_DELAY. F5_MAX_UNACKED caps the
# queue's per-flow delivery window low so txUnackedMsgCount pins near the
# ceiling and spooledMsgCount grows — the queue-level signals SOL-150344
# asserts, replacing the per-client slowSubscriber flag (SOL-150328).
F5_QUEUE="test-queue-slow-consumer"
F5_TOPIC="e2e-monitoring/slow-consumer/topic"
F5_PUBLISH_RATE=100   # msg/s into the queue's topic
F5_PUBLISH_SIZE=256   # bytes per message
F5_ACK_DELAY="2s"     # delay before ACKing each message (the throttle)
F5_MAX_UNACKED=10     # maxDeliveredUnackedMsgsPerFlow on the F5 queue
# txUnackedMsgCount oscillates by one as the slow consumer ACKs, so the
# "pinned near the ceiling" assertion uses 80% of the per-flow cap rather than
# exact equality. Shared by verify-fixtures.sh (SEMP-direct) and
# test-monitoring-tools.sh (MCP-tool) so both layers assert against the same threshold.
F5_NEAR_UNACKED=$(( F5_MAX_UNACKED * 8 / 10 ))

# Provisions F5_QUEUE with a low per-flow unacked window and a subscription to
# F5_TOPIC, then spawns a long-lived broker-driver slow-consumer that floods the
# topic and ACKs slowly. The process self-writes a PID file that
# stop_broker_drivers reaps; the queue is dropped in cleanup (F5 owns it, unlike
# F3/F4 which reuse test-queue).
create_slow_consumer_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    local broker_letter="$4"     # "a" or "b" — resolves SMF host:port in broker-driver
    local pidfile="$BIN_DIR/broker-driver-f5-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f5-$broker_letter.log"
    log_info "Creating slow-consumer fixture on $label (queue=$F5_QUEUE maxUnacked=$F5_MAX_UNACKED ackDelay=$F5_ACK_DELAY) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F5_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true,\"maxDeliveredUnackedMsgsPerFlow\":$F5_MAX_UNACKED}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F5_TOPIC\"}" >/dev/null

    # nohup + setsid so the driver survives an aborted parent shell; the harness
    # still finds it via the pidfile glob (broker-driver-f*.pid).
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" slow-consumer \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --queue="$F5_QUEUE" \
        --topic="$F5_TOPIC" \
        --rate="$F5_PUBLISH_RATE" \
        --size="$F5_PUBLISH_SIZE" \
        --ack-delay="$F5_ACK_DELAY" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver slow-consumer" || return 1

    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE"
    log_info "Slow-consumer fixture started on $label (PID=$(<"$pidfile"))"
}

# Drops the F5 queue (cascades its topic subscription). The driver is reaped by
# stop_broker_drivers first, so the bind is gone before the queue delete.
cleanup_slow_consumer_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up slow-consumer fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE"
}

# F6 slow-DIRECT-subscriber constants. Distinct from F5 (queue slow-consumer):
# F6 flips the per-client `slowSubscriber` flag that list-slow-subscribers
# filters on. The flag tracks TCP egress back-pressure, which a slow-ACK
# guaranteed consumer never triggers (SOL-150328) — so we close the client's
# receive window at the OS level by SIGSTOPping the subscriber process while a
# separate publisher floods its subscribed topic with large payloads. Direct
# messaging (no queue/spool) so the broker has nowhere to spool the overflow and
# egress congestion is forced onto the stalled client.
F6_SUB_CLIENT_NAME_A="e2e-monitoring-slow-subscriber-a"
F6_SUB_CLIENT_NAME_B="e2e-monitoring-slow-subscriber-b"
F6_TOPIC="e2e-monitoring/slow-subscriber/topic"
F6_FLOOD_RATE=3000     # msg/s — high enough to keep the egress window pinned
F6_FLOOD_SIZE=50000    # bytes — large payloads fill the window fast
F6_FLAG_TIMEOUT=60     # max seconds to wait for slowSubscriber to flip true

# Polls the broker until a client's slowSubscriber flag reads true, or fails
# after F6_FLAG_TIMEOUT. The flag is computed over a rolling ~1 min window, so
# a single read just after SIGSTOP flakes — poll until it settles true.
#
# Self-healing: if the broker reports the client absent (HTTP 400 NOT_FOUND),
# the SIGSTOPped subscriber was reaped by the broker (keepalive timeout or
# egress threshold) — respawn it once and keep polling. The `recreated` flag
# allows exactly one respawn per call to prevent an endless respawn loop; a
# second reap falls back to normal once-per-second polling and ultimately
# times out via the existing log_fail. Respawning resets `attempt` so the new
# subscriber gets its own full F6_FLAG_TIMEOUT budget to flip the flag.
#   $1 broker_url   $2 label   $3 client_name   $4 broker_letter ("a"|"b")
wait_for_slow_subscriber() {
    local broker_url="$1"
    local label="$2"
    local client_name="$3"
    local broker_letter="$4"
    local attempt=0 recreated=0 flag body http_status tx_rate tx_discards prev_observation=""
    local url="$broker_url/SEMP/v2/__private_monitor__/msgVpns/$BROKER_VPN/clients/$client_name?select=slowSubscriber,txByteRate,txDiscardedMsgCount"
    while [ $attempt -lt "$F6_FLAG_TIMEOUT" ]; do
        # http_status drives the self-heal branch below: 400 NOT_FOUND means the
        # broker reaped the client and we must respawn, vs 200/flag=false where
        # the client is present and the flag just decayed.
        # txByteRate + txDiscardedMsgCount disambiguate flag=false further:
        # rate=0 + stable discards = broker drained the egress (real decay);
        # rate>0 or rising discards = broker still delivering/dropping
        # (flag wrong, or just a momentary dip).
        body=$(curl -s -o - -w '\n__HTTP_STATUS__%{http_code}' \
            -u "$BROKER_USER:$BROKER_PASS" "$url" 2>/dev/null) || true
        http_status="${body##*__HTTP_STATUS__}"
        body="${body%__HTTP_STATUS__*}"
        # `|| flag=""` so a transient non-JSON body (jq exits non-zero) just
        # retries on the next poll rather than aborting the run under `set -e`.
        flag=$(echo "$body" | jq -r '.data.slowSubscriber // empty' 2>/dev/null) || flag=""
        tx_rate=$(echo "$body" | jq -r '.data.txByteRate // empty' 2>/dev/null) || tx_rate=""
        tx_discards=$(echo "$body" | jq -r '.data.txDiscardedMsgCount // empty' 2>/dev/null) || tx_discards=""
        local observation="http=$http_status flag=${flag:-<empty>} txByteRate=${tx_rate:-<empty>} txDiscardedMsgCount=${tx_discards:-<empty>}"
        if [ "$attempt" = "0" ] || [ "$observation" != "$prev_observation" ]; then
            log_info "  poll [$label/$client_name] t=${attempt}s $observation"
            prev_observation="$observation"
        fi
        if [ "$flag" = "true" ]; then
            log_info "  slowSubscriber=true for $client_name on $label (${attempt}s)"
            return 0
        fi
        # Broker returns 400 NOT_FOUND when the client is gone. The flood
        # publisher is still running, so respawning the SIGSTOPped subscriber
        # alone is enough to re-arm the fixture. Reset the attempt counter so
        # the new subscriber gets its own full timeout budget to flip the flag.
        if [ "$http_status" = "400" ] && [ "$recreated" = "0" ]; then
            recreated=1
            respawn_slow_subscriber_on "$label" "$broker_letter" "$client_name" || return 1
            attempt=0
            prev_observation=""
            continue
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "F6 [$label]: slowSubscriber did not flip true for $client_name within ${F6_FLAG_TIMEOUT}s (last observation: $prev_observation)"
    return 1
}

# Spawns one F6 slow-direct-subscriber driver and waits for its pidfile.
# Shared between the initial fixture create and the post-reap respawn.
#   $1 broker_letter   $2 client_name   $3 pidfile   $4 logfile   $5 label   $6 desc
_spawn_slow_direct_subscriber() {
    local broker_letter="$1" client_name="$2" pidfile="$3" logfile="$4" label="$5" desc="$6"
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" slow-direct-subscriber \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --topic="$F6_TOPIC" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &
    wait_for_pidfile "$pidfile" "$label" "$logfile" "$desc"
}

# Respawns just the SIGSTOPped subscriber half of the F6 fixture after the
# broker has reaped it. The flood publisher is unaffected, so we don't touch
# it (a second publisher with the same client-name would let the broker evict
# the live one, breaking egress pressure). Returns once the new subscriber is
# spawned and SIGSTOPped; the caller (wait_for_slow_subscriber) keeps polling
# for the flag to flip.
respawn_slow_subscriber_on() {
    local label="$1" broker_letter="$2" client_name="$3"
    local sub_pidfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.pid"
    local sub_logfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.log"
    log_info "  client $client_name absent on $label (http=400) — broker reaped it; respawning subscriber"
    # The old subscriber is still alive locally (SIGSTOPped, just disconnected
    # from the broker). Reap it BEFORE reusing the pidfile path: the driver
    # has `defer os.Remove(*pidfile)` (see broker-driver/slow_subscriber.go),
    # so a late-exiting old process would otherwise delete the new pidfile we
    # write to the canonical path.
    if [ -f "$sub_pidfile" ]; then
        local old_pid
        old_pid=$(<"$sub_pidfile")
        # Best-effort identity check before signalling: if the PID has been
        # recycled to an unrelated process, /proc/$pid/exe won't resolve to
        # our broker-driver binary. Readlink fails silently for processes we
        # don't own; on mismatch we skip signalling rather than risk killing
        # something else.
        if kill -0 "$old_pid" 2>/dev/null; then
            local exe
            exe=$(readlink "/proc/$old_pid/exe" 2>/dev/null || true)
            if [ -z "$exe" ] || [ "$exe" = "$BIN_DIR/broker-driver" ]; then
                # SIGCONT first so a SIGSTOPped process can actually receive
                # the SIGTERM kill_gracefully sends; otherwise TERM is held
                # pending until CONT and the 5s grace window burns to SIGKILL.
                kill -CONT "$old_pid" 2>/dev/null || true
                kill_gracefully "$old_pid"
            else
                log_warn "  PID $old_pid in $sub_pidfile is not broker-driver (exe=$exe); skipping signal"
            fi
        fi
        rm -f "$sub_pidfile"
    fi
    # Old process is fully exited and the canonical pidfile is gone, so the
    # replacement can write directly onto it — no temp-path + rename dance.
    _spawn_slow_direct_subscriber "$broker_letter" "$client_name" "$sub_pidfile" "$sub_logfile" \
        "$label" "broker-driver slow-direct-subscriber (respawn)" || return 1
    kill -STOP "$(<"$sub_pidfile")"
    log_info "  SIGSTOP sent to respawned slow-subscriber (PID=$(<"$sub_pidfile")) on $label"
}

# Provisions F6: a long-lived direct subscriber on F6_TOPIC plus a separate
# flood publisher into that topic, then SIGSTOPs the subscriber so its TCP
# receive window stalls and the broker flags it slowSubscriber=true. Two PIDs
# (sub + pub) so SIGSTOP halts only the subscriber, never the flood. Both
# pidfiles match the broker-driver-f*.pid glob and are reaped by
# stop_broker_drivers (which SIGCONTs first). No queue — direct messaging only.
create_slow_subscriber_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"     # "a" or "b" — resolves SMF host:port in broker-driver
    local client_name="$4"
    local sub_pidfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.pid"
    local sub_logfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.log"
    local pub_pidfile="$BIN_DIR/broker-driver-f6-pub-$broker_letter.pid"
    local pub_logfile="$BIN_DIR/broker-driver-f6-pub-$broker_letter.log"
    log_info "Creating slow-subscriber fixture on $label (clientName=$client_name topic=$F6_TOPIC) ..."

    # Direct subscriber (this is the process we SIGSTOP).
    _spawn_slow_direct_subscriber "$broker_letter" "$client_name" "$sub_pidfile" "$sub_logfile" \
        "$label" "broker-driver slow-direct-subscriber" || return 1

    # Separate large-payload flood into the subscribed topic — must NOT be
    # stopped, so it keeps egress pressure on the stalled subscriber.
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" publisher \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="e2e-monitoring-slow-sub-flood-$broker_letter" \
        --topic="$F6_TOPIC" \
        --rate="$F6_FLOOD_RATE" \
        --size="$F6_FLOOD_SIZE" \
        --pidfile="$pub_pidfile" \
        >"$pub_logfile" 2>&1 &
    wait_for_pidfile "$pub_pidfile" "$label" "$pub_logfile" "broker-driver publisher (F6 flood)" || return 1

    # Close the subscriber's TCP receive window at the OS level. A slow app
    # callback is not enough (the client C lib drains the socket regardless);
    # SIGSTOP halts the whole process so the window stays shut.
    kill -STOP "$(<"$sub_pidfile")"
    log_info "  SIGSTOP sent to slow-subscriber (PID=$(<"$sub_pidfile")) on $label"

    # Wait for the broker to observe the stall and flip the flag.
    wait_for_slow_subscriber "$broker_url" "$label" "$client_name" "$broker_letter" || return 1
    log_info "Slow-subscriber fixture created on $label"
}

# Both F6 processes are reaped by stop_broker_drivers via the pidfile glob, and
# it SIGCONTs the stopped subscriber before SIGTERM, so teardown needs nothing
# here. No queue to delete (direct messaging). Kept as a label-only nop for
# symmetry with the other create/cleanup pairs.
cleanup_slow_subscriber_on() {
    local label="$1"
    log_info "Slow-subscriber cleanup on $label deferred to stop_broker_drivers"
}

# F7-spool constants.
F7_SPOOL_QUEUE="test-queue-discards-spool"
F7_SPOOL_TOPIC="e2e-monitoring/discards/spool"
F7_SPOOL_COUNT=8000   # × 256 B ≈ 2 MB; overflows the 1 MB spool quota
F7_SPOOL_SIZE=256
F7_SPOOL_MAX_MB=1     # maxMsgSpoolUsage in MB

# Provisions test-queue-discards-spool with a 1 MB spool cap and
# egressEnabled=false, then runs a one-shot publish-batch that fills ~2 MB
# worth of messages. The broker discards the overflow and increments
# maxMsgSpoolUsageExceededDiscardedMsgCount — a cumulative counter, so no
# sustained traffic is needed after the one-shot publish.
create_discard_spool_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    local broker_letter="$4"
    log_info "Creating discard-spool fixture on $label (queue=$F7_SPOOL_QUEUE maxSpool=${F7_SPOOL_MAX_MB}MB) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F7_SPOOL_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":false,\"maxMsgSpoolUsage\":$F7_SPOOL_MAX_MB}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_SPOOL_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F7_SPOOL_TOPIC\"}" >/dev/null

    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" \
        --topic="$F7_SPOOL_TOPIC" \
        --count="$F7_SPOOL_COUNT" \
        --size="$F7_SPOOL_SIZE" \
        --message-type=persistent

    log_info "Discard-spool fixture created on $label"
}

# Drops the F7-spool queue (cascades to its topic subscription).
cleanup_discard_spool_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up discard-spool fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_SPOOL_QUEUE"
}

# F7-ttl constants.
F7_TTL_QUEUE="test-queue-discards-ttl"
F7_TTL_TOPIC="e2e-monitoring/discards/ttl"
F7_TTL_COUNT=200     # small batch; messages expire by TTL, not spool
F7_TTL_SIZE=256
F7_TTL_MAX_TTL_S=1   # maxTtl in seconds
F7_TTL_WAIT_S=2      # sleep after publish to let the 1 s TTL expire

# Provisions test-queue-discards-ttl with a 1 s TTL and no consumer, publishes
# a one-shot batch with --dmq-eligible=false so the broker increments
# maxTtlExpiredDiscardedMsgCount rather than moving expired messages to the DMQ.
# Sleeps F7_TTL_WAIT_S after publishing so the TTL window closes before
# verify-fixtures.sh runs the AC 9 assertion.
create_discard_ttl_on() {
    local semp_config="$1"
    local label="$2"
    local broker_letter="$3"
    log_info "Creating discard-ttl fixture on $label (queue=$F7_TTL_QUEUE maxTtl=${F7_TTL_MAX_TTL_S}s) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F7_TTL_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true,\"maxTtl\":$F7_TTL_MAX_TTL_S,\"respectTtlEnabled\":true}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_TTL_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F7_TTL_TOPIC\"}" >/dev/null

    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" \
        --topic="$F7_TTL_TOPIC" \
        --count="$F7_TTL_COUNT" \
        --size="$F7_TTL_SIZE" \
        --message-type=persistent \
        --dmq-eligible=false

    log_info "Waiting ${F7_TTL_WAIT_S}s for TTL to expire ..."
    sleep "$F7_TTL_WAIT_S"
    log_info "Discard-ttl fixture created on $label"
}

# Drops the F7-ttl queue (cascades to its topic subscription).
cleanup_discard_ttl_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up discard-ttl fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_TTL_QUEUE"
}

# Epoch (seconds since Unix) at which the last F4 publisher finished
# starting. verify-fixtures.sh reads this to wait out the AC 5 settle
# window (≥ 25 s) before sampling rxMsgRate / txMsgRate. Exported so the
# child verifier process inherits it. Use := so sourcing helpers.sh in the
# child verifier does not clobber the value exported by the parent runner.
: "${F4_READY_EPOCH:=}"
export F4_READY_EPOCH

# Epoch at which the F5 slow-consumer drivers finished starting.
# verify-fixtures.sh reads this to wait out the F5 settle window before
# sampling the queue-level signals. Same := guard as F4_READY_EPOCH so a
# child verifier sourcing helpers.sh does not clobber the parent's value.
: "${F5_READY_EPOCH:=}"
export F5_READY_EPOCH

create_fixtures() {
    cleanup_fixtures
    # NFR-4: one-shot SEMP (F1, F2) before client-bearing (F3+) so the
    # queues a receiver binds to are already provisioned.
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_connected_client_on "broker-a" "$BROKER_A_URL" a "$F3_CLIENT_NAME_A"
    create_connected_client_on "broker-b" "$BROKER_B_URL" b "$F3_CLIENT_NAME_B"
    create_sustained_traffic_on "broker-a" "$BROKER_A_URL" a
    create_sustained_traffic_on "broker-b" "$BROKER_B_URL" b
    F4_READY_EPOCH=$(date +%s)
    export F4_READY_EPOCH
    # F5 owns its queue and is independent of F3/F4; its consumer binds to that
    # queue so it must be reaped before the queue delete (cleanup handles order).
    create_slow_consumer_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL" a
    create_slow_consumer_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL" b
    F5_READY_EPOCH=$(date +%s)
    export F5_READY_EPOCH
    # F6 is independent of F5; it owns no queue (direct messaging). create_*
    # blocks until the slowSubscriber flag has flipped, so no settle epoch is
    # needed — the tool test can read the flag immediately afterwards.
    create_slow_subscriber_on "broker-a" "$BROKER_A_URL" a "$F6_SUB_CLIENT_NAME_A"
    create_slow_subscriber_on "broker-b" "$BROKER_B_URL" b "$F6_SUB_CLIENT_NAME_B"
    # F7 is independent of F3/F4 — run after them but the queues have no
    # client dependency so order within F7 does not matter.
    create_discard_spool_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL" a
    create_discard_spool_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL" b
    create_discard_ttl_on "$BROKER_A_SEMP_CONFIG" "broker-a" a
    create_discard_ttl_on "$BROKER_B_SEMP_CONFIG" "broker-b" b
}

cleanup_fixtures() {
    # Reap client-bearing fixtures first — broker refuses to delete a queue
    # while a client is bound, so stop_broker_drivers must run before any
    # SEMP queue/RDP deletes downstream.
    stop_broker_drivers
    cleanup_discard_ttl_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_discard_ttl_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_discard_spool_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_discard_spool_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_slow_consumer_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_slow_consumer_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_slow_subscriber_on "broker-a"
    cleanup_slow_subscriber_on "broker-b"
    cleanup_sustained_traffic_on "broker-a"
    cleanup_sustained_traffic_on "broker-b"
    cleanup_connected_client_on "broker-a"
    cleanup_connected_client_on "broker-b"
    cleanup_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}
