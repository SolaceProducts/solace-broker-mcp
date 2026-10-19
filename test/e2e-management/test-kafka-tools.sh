#!/usr/bin/env bash
# Kafka bridge write-tool tests (SOL-156214), driven over the MCP JSON-RPC wire
# against real brokers and a real Kafka (docker-compose.yml: the `kafka` service,
# PLAINTEXT on 9092 and SASL_PLAINTEXT/PLAIN on 9094).
#
# Covers create-/update-/delete-kafka-receiver, create-/delete-kafka-receiver-topic-binding,
# create-/update-/delete-kafka-sender and create-/delete-kafka-sender-queue-binding:
#   - both modes: no authentication (created and enabled in one call), and
#     authentication with the secret set OUT OF BAND (the test PATCHes it straight
#     into SEMP, as the user would; it never goes through the MCP server);
#   - the server-side guards: every write-only secret rejected in every param shape,
#     the enabled/scheme rules, and that no secret value reaches the server log;
#   - broker behaviour the tool descriptions teach: a bridge cannot be deleted
#     while it has bindings, a sender binding to a missing Kafka topic stays down
#     with a reason and recovers when the topic appears;
#   - outcomes (already_exists with its hint, already_absent) and annotations.
#
# Fixture model: per-test ownership, like test-management-tools.sh. Every object is
# named e2e-config-kafka-*, and the suite sweep (helpers.sh) removes any remnant
# bindings-first on entry and on exit.
#
# Status checks poll with a bounded deadline instead of fixed sleeps: a bridge takes
# 10-40 s to come up after it is enabled, which is exactly what the tools' nextStep
# tells an agent to expect.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

trap sweep_config_fixtures EXIT
sweep_config_fixtures
wait_for_kafka

# ── Helpers ──────────────────────────────────────────────────────────────────

broker_url_for() {
    case "$1" in
        broker-a) echo "$BROKER_A_URL" ;;
        broker-b) echo "$BROKER_B_URL" ;;
        *) echo "broker_url_for: unknown broker alias: $1" >&2; return 1 ;;
    esac
}

semp_config_for() {
    case "$1" in
        broker-a) echo "$BROKER_A_SEMP_CONFIG" ;;
        broker-b) echo "$BROKER_B_SEMP_CONFIG" ;;
        *) echo "semp_config_for: unknown broker alias: $1" >&2; return 1 ;;
    esac
}

# Call a tool and capture the outcome in globals:
#   KT_ERR      "true" if the tool result is flagged isError
#   KT_TEXT     the result's text content (the error message when KT_ERR is true)
#   KT_CONTENT  the structured result (empty when KT_ERR is true)
# Returns non-zero only on a transport failure.
kt() {
    local tool="$1" args="$2"
    KT_RESP=$(mcp_call_tool "$tool" "$args") || { log_fail "transport failure calling $tool"; return 1; }
    KT_ERR=$(jq -r '.result.isError // false' <<<"$KT_RESP")
    KT_TEXT=$(jq -r '.result.content[0].text // ""' <<<"$KT_RESP")
    KT_CONTENT=""
    if [ "$KT_ERR" != "true" ]; then
        KT_CONTENT=$(extract_content "$KT_RESP")
    fi
}

# kt, then assert the call succeeded.   $1 tool  $2 args  $3 description
kt_ok() {
    kt "$1" "$2" || return 1
    if [ "$KT_ERR" = "true" ]; then
        log_fail "$3: $1 returned isError=true: $KT_TEXT"
        return 1
    fi
}

# kt, then assert the call was rejected; the message is left in KT_TEXT.
kt_rejected() {
    kt "$1" "$2" || return 1
    if [ "$KT_ERR" != "true" ]; then
        log_fail "$3: $1 should have been rejected but succeeded: $KT_CONTENT"
        return 1
    fi
}

# Poll a status tool until a jq predicate over its structured result holds.
#   $1 tool  $2 args  $3 jq predicate  $4 timeout seconds  $5 description
wait_status() {
    local tool="$1" args="$2" pred="$3" timeout="$4" desc="$5"
    local deadline=$((SECONDS + timeout))
    while [ "$SECONDS" -lt "$deadline" ]; do
        kt "$tool" "$args" || return 1
        if [ "$KT_ERR" != "true" ] && [ "$(jq -r "$pred" <<<"$KT_CONTENT" 2>/dev/null)" = "true" ]; then
            return 0
        fi
        sleep 3
    done
    log_fail "$desc: not satisfied within ${timeout}s: $pred; last: ${KT_CONTENT:-$KT_TEXT}"
    return 1
}

# Assert the MCP server's log does not contain a value. It first requires the log to
# be non-empty and to contain a line the server certainly wrote for these calls, so the
# check cannot pass vacuously (a missing file, an empty capture, a log level that hides
# the lines it is meant to inspect).
#   $1 value  $2 description
assert_server_log_lacks() {
    local value="$1" desc="$2"
    if [ ! -s "$MCP_SERVER_LOG" ]; then
        log_fail "$desc: the MCP server log ($MCP_SERVER_LOG) is missing or empty, so this check would prove nothing"
        return 1
    fi
    if ! grep -q "tool invoked" "$MCP_SERVER_LOG"; then
        log_fail "$desc: no 'tool invoked' line in the MCP server log, so this check would prove nothing"
        return 1
    fi
    if grep -qF -- "$value" "$MCP_SERVER_LOG"; then
        log_fail "$desc: the value appears in the MCP server log"
        return 1
    fi
}

# Assert a rejected secret call: it names the field, says to rotate an exposed secret,
# and never echoes the value. Reads the globals kt_rejected set.
#   $1 secret field  $2 sentinel value  $3 description
assert_secret_rejection() {
    local secret="$1" sentinel="$2" desc="$3"
    assert_contains "$KT_TEXT" "\"$secret\"" "$desc: the rejection names $secret" || return 1
    assert_contains "$KT_TEXT" "rotate" "$desc: the rejection says to rotate an exposed secret" || return 1
    assert_not_contains "$KT_TEXT" "$sentinel" "$desc: the secret value is not echoed" || return 1
}

mk_queue() { # broker name
    kt_ok "create-queue" "$(jq -nc --arg b "$1" --arg n "$2" \
        '{broker:$b,msgVpnName:"default",queueName:$n,queueConfig:{accessType:"exclusive",permission:"consume",ingressEnabled:true,egressEnabled:true}}')" \
        "create queue $2 [$1]"
}

# ── No authentication: Kafka -> Solace ───────────────────────────────────────

test_kafka_receiver_none_roundtrip() {
    local broker="$1"
    local rx="e2e-config-kafka-rx-none-$broker" q="e2e-config-kafka-q-in-$broker"
    local topic="e2e-config-kafka-in-$broker" lt="e2e-config/kafka/in/$broker"
    local burl semp
    burl=$(broker_url_for "$broker"); semp=$(semp_config_for "$broker")
    kafka_create_topic "$topic" || { log_fail "could not create Kafka topic $topic [$broker]"; return 1; }

    mk_queue "$broker" "$q" || return 1
    kt_ok "create-queue-subscription" "$(jq -nc --arg b "$broker" --arg q "$q" --arg t "$lt/>" \
        '{broker:$b,msgVpnName:"default",queueName:$q,subscriptionTopic:$t}')" "subscription [$broker]" || return 1

    # Created and enabled in one call; the result points at the status tool.
    local create_args
    create_args=$(jq -nc --arg b "$broker" --arg n "$rx" --arg bs "$KAFKA_BOOTSTRAP" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"none",bootstrapAddressList:$bs,enabled:true}}')
    kt_ok "create-kafka-receiver" "$create_args" "none-mode create [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.summary.secretRequired' "false" "none mode [$broker]: no secret is needed" || return 1
    assert_json_field "$KT_CONTENT" '.summary.enabled' "true" "none mode [$broker]: created enabled" || return 1
    assert_contains "$(jq -r '.summary.nextStep' <<<"$KT_CONTENT")" "get-kafka-receiver-status" \
        "none mode [$broker]: nextStep points at the status tool" || return 1

    # A duplicate create is a desired-state noop that tells the agent to look before enabling.
    kt_ok "create-kafka-receiver" "$create_args" "duplicate create [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.outcome' "already_exists" "duplicate create [$broker]: already_exists" || return 1
    assert_contains "$(jq -r '.message' <<<"$KT_CONTENT")" "get-kafka-receiver-status" \
        "duplicate create [$broker]: the outcome carries the scheme hint" || return 1

    kt_ok "create-kafka-receiver-topic-binding" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg t "$topic" --arg lt "$lt/x" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,topicName:$t,topicBindingConfig:{localTopic:$lt,initialOffset:"beginning",enabled:true}}')" \
        "binding [$broker]" || return 1

    # The broker refuses to delete a receiver that still has a binding, and says so verbatim.
    kt_rejected "delete-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n}')" \
        "delete with bindings [$broker]" || return 1
    assert_contains "$KT_TEXT" "Cannot delete Kafka Receiver" "delete with bindings [$broker]: the broker's refusal reaches the agent" || return 1

    wait_status "get-kafka-receiver-status" "$(jq -nc --arg b "$broker" --arg n "$rx" '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n}')" \
        '.kafkaReceiverStatus.data.up == true and .kafkaReceiverStatus.data.topicBindingUpCount == 1' 150 \
        "receiver and binding up [$broker]" || return 1

    # Records flow Kafka -> Solace queue.
    kafka_produce "$topic" "k1:v1" "k2:v2" || { log_fail "none mode [$broker]: could not produce records to Kafka topic $topic"; return 1; }
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/queues/$q" 90 '.data.spooledMsgCount >= 2' \
        || { log_fail "none mode [$broker]: records produced to Kafka did not reach $q"; return 1; }

    # Tear down bindings first.
    kt_ok "delete-kafka-receiver-topic-binding" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg t "$topic" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,topicName:$t}')" "delete binding [$broker]" || return 1
    kt_ok "delete-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n}')" \
        "delete receiver [$broker]" || return 1
    if semp_monitor_get "$burl" "msgVpns/$BROKER_VPN/kafkaReceivers/$rx" >/dev/null 2>&1; then
        log_fail "none mode [$broker]: receiver $rx still exists after delete"; return 1
    fi
    kt_ok "delete-queue" "$(jq -nc --arg b "$broker" --arg n "$q" '{broker:$b,msgVpnName:"default",queueName:$n}')" "cleanup queue [$broker]" || return 1
}

# ── Authentication: the secret is set out of band ────────────────────────────

test_kafka_receiver_auth_roundtrip() {
    local broker="$1"
    local rx="e2e-config-kafka-rx-auth-$broker" topic="e2e-config-kafka-in-$broker"
    local burl semp
    burl=$(broker_url_for "$broker"); semp=$(semp_config_for "$broker")
    kafka_create_topic "$topic" || { log_fail "could not create Kafka topic $topic [$broker]"; return 1; }
    local status_args
    status_args=$(jq -nc --arg b "$broker" --arg n "$rx" '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n}')

    # Created disabled with the real scheme and the non-secret username.
    kt_ok "create-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg bs "$KAFKA_BOOTSTRAP_SASL" --arg u "$KAFKA_SASL_USER" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"basic",authenticationBasicUsername:$u,bootstrapAddressList:$bs}}')" \
        "auth-mode create [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.summary.secretRequired' "true" "auth mode [$broker]: a secret is required" || return 1
    assert_json_field "$KT_CONTENT" '.summary.enabled' "false" "auth mode [$broker]: created disabled" || return 1
    assert_json_field "$KT_CONTENT" '.summary.secretFields | index("authenticationBasicPassword") != null' "true" \
        "auth mode [$broker]: the handoff names the password attribute" || return 1
    assert_contains "$(jq -r '.summary.handoff' <<<"$KT_CONTENT")" "Do not paste the secret into this chat" \
        "auth mode [$broker]: the handoff tells the user not to paste the secret" || return 1

    kt_ok "create-kafka-receiver-topic-binding" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg t "$topic" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,topicName:$t,topicBindingConfig:{localTopic:"e2e-config/kafka/auth",enabled:true}}')" \
        "binding [$broker]" || return 1

    # The user says "done" too early: enabling without the secret is accepted (a
    # single-step update) and the status says exactly what is missing.
    kt_ok "update-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{enabled:true}}')" "premature enable [$broker]" || return 1
    assert_contains "$(jq -r '.summary.nextStep' <<<"$KT_CONTENT")" "get-kafka-receiver-status" \
        "premature enable [$broker]: nextStep points at the status tool" || return 1
    wait_status "get-kafka-receiver-status" "$status_args" '.kafkaReceiverStatus.data.failureReason == "Username or password missing"' 60 \
        "missing secret is reported [$broker]" || return 1

    # OUT OF BAND: the user sets username and password together, straight into SEMP.
    semp_patch "$semp" "msgVpns/$BROKER_VPN/kafkaReceivers/$rx" \
        "$(jq -nc --arg u "$KAFKA_SASL_USER" --arg p "$KAFKA_SASL_PASSWORD" '{authenticationBasicUsername:$u,authenticationBasicPassword:$p}')" \
        || { log_fail "auth mode [$broker]: could not set the secret out of band"; return 1; }
    wait_status "get-kafka-receiver-status" "$status_args" \
        '.kafkaReceiverStatus.data.up == true and .kafkaReceiverStatus.data.topicBindingUpCount == 1' 150 \
        "authenticated receiver and binding up [$broker]" || return 1

    # The broker never returns the secret, and the MCP server never saw it.
    local config_body
    config_body=$(semp_curl -sf "$semp/msgVpns/$BROKER_VPN/kafkaReceivers/$rx") \
        || { log_fail "auth mode [$broker]: could not read $rx back from SEMP"; return 1; }
    if [ "$(jq '.data | has("authenticationBasicPassword")' <<<"$config_body")" = "true" ]; then
        log_fail "auth mode [$broker]: SEMP returned the write-only password"; return 1
    fi
    # The secret only ever went straight to SEMP, so it must not be in the server's log
    # (defence in depth; the rejection test below covers secrets sent through the tools).
    assert_server_log_lacks "$KAFKA_SASL_PASSWORD" "auth mode [$broker]" || return 1

    kt_ok "delete-kafka-receiver-topic-binding" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg t "$topic" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,topicName:$t}')" "delete binding [$broker]" || return 1
    kt_ok "delete-kafka-receiver" "$status_args" "delete receiver [$broker]" || return 1
}

# ── Solace -> Kafka, including a missing Kafka topic ─────────────────────────

test_kafka_sender_roundtrip() {
    local broker="$1"
    local tx="e2e-config-kafka-tx-$broker" q="e2e-config-kafka-q-out-$broker" q2="e2e-config-kafka-q-late-$broker"
    local topic="e2e-config-kafka-out-$broker" late="e2e-config-kafka-late-$broker"
    local burl
    burl=$(broker_url_for "$broker")
    kafka_create_topic "$topic" || { log_fail "could not create Kafka topic $topic [$broker]"; return 1; }
    local sender_args
    sender_args=$(jq -nc --arg b "$broker" --arg n "$tx" '{broker:$b,msgVpnName:"default",kafkaSenderName:$n}')

    mk_queue "$broker" "$q" || return 1
    mk_queue "$broker" "$q2" || return 1
    kt_ok "create-kafka-sender" "$(jq -nc --arg b "$broker" --arg n "$tx" --arg bs "$KAFKA_BOOTSTRAP" \
        '{broker:$b,msgVpnName:"default",kafkaSenderName:$n,kafkaSenderConfig:{authenticationScheme:"none",bootstrapAddressList:$bs,enabled:true}}')" \
        "sender create [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.summary.object' "kafkaSender" "sender [$broker]: result describes a sender" || return 1
    assert_contains "$(jq -r '.summary.nextStep' <<<"$KT_CONTENT")" "get-kafka-sender-status" \
        "sender [$broker]: nextStep points at the sender status tool" || return 1

    # One binding to a topic that exists, one to a topic that does not (yet). The
    # broker accepts both; the second stays down with a reason.
    kt_ok "create-kafka-sender-queue-binding" "$(jq -nc --arg b "$broker" --arg n "$tx" --arg q "$q" --arg t "$topic" \
        '{broker:$b,msgVpnName:"default",kafkaSenderName:$n,queueName:$q,queueBindingConfig:{remoteTopic:$t,ackMode:"all",enabled:true}}')" \
        "binding to an existing topic [$broker]" || return 1
    kt_ok "create-kafka-sender-queue-binding" "$(jq -nc --arg b "$broker" --arg n "$tx" --arg q "$q2" --arg t "$late" \
        '{broker:$b,msgVpnName:"default",kafkaSenderName:$n,queueName:$q,queueBindingConfig:{remoteTopic:$t,enabled:true}}')" \
        "binding to a missing topic [$broker]" || return 1

    wait_status "get-kafka-sender-status" "$sender_args" \
        '.kafkaSenderStatus.data.up == true and .kafkaSenderStatus.data.queueBindingCount == 2' 90 \
        "sender up with two bindings [$broker]" || return 1
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/kafkaSenders/$tx/queueBindings/$q" 150 '.data.up == true' \
        || { log_fail "sender [$broker]: the binding to an existing topic did not come up"; return 1; }
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/kafkaSenders/$tx/queueBindings/$q2" 60 \
        '.data.up == false and (.data.failureReason | test("no leader"))' \
        || { log_fail "sender [$broker]: the binding to a missing topic should be down with 'no leader'"; return 1; }

    # The topic appears; the binding recovers by itself.
    kafka_create_topic "$late" || { log_fail "could not create Kafka topic $late [$broker]"; return 1; }
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/kafkaSenders/$tx/queueBindings/$q2" 150 '.data.up == true' \
        || { log_fail "sender [$broker]: the binding did not recover after the topic was created"; return 1; }

    # Bindings must go first.
    kt_rejected "delete-kafka-sender" "$sender_args" "delete with bindings [$broker]" || return 1
    assert_contains "$KT_TEXT" "Cannot delete Kafka Sender" "delete with bindings [$broker]: the broker's refusal reaches the agent" || return 1
    local queue
    for queue in "$q" "$q2"; do
        kt_ok "delete-kafka-sender-queue-binding" "$(jq -nc --arg b "$broker" --arg n "$tx" --arg q "$queue" \
            '{broker:$b,msgVpnName:"default",kafkaSenderName:$n,queueName:$q}')" "delete binding $queue [$broker]" || return 1
    done
    kt_ok "delete-kafka-sender" "$sender_args" "delete sender [$broker]" || return 1
    for queue in "$q" "$q2"; do
        kt_ok "delete-queue" "$(jq -nc --arg b "$broker" --arg n "$queue" '{broker:$b,msgVpnName:"default",queueName:$n}')" "cleanup $queue [$broker]" || return 1
    done
}

# ── Secrets never go through the MCP server ──────────────────────────────────

test_kafka_secret_rejection() {
    local broker="$1"
    local sentinel="E2E-SECRET-SENTINEL-$RANDOM$RANDOM"
    local secrets=(authenticationAwsMskIamSecretAccessKey authenticationBasicPassword authenticationClientCertContent
                   authenticationClientCertPassword authenticationKerberosKeytabContent authenticationOauthClientSecret
                   authenticationScramPassword)
    local kind tool_noun name_param config_param secret
    for kind in receiver sender; do
        tool_noun="kafka-$kind"
        if [ "$kind" = "receiver" ]; then name_param="kafkaReceiverName"; config_param="kafkaReceiverConfig"
        else name_param="kafkaSenderName"; config_param="kafkaSenderConfig"; fi
        local name="e2e-config-kafka-rej-$kind-$broker"
        for secret in "${secrets[@]}"; do
            # create, secret inside the config object
            kt_rejected "create-$tool_noun" "$(jq -nc --arg b "$broker" --arg n "$name" --arg np "$name_param" --arg cp "$config_param" --arg s "$secret" --arg v "$sentinel" \
                '{broker:$b,msgVpnName:"default"} + {($np):$n} + {($cp):{($s):$v}}')" "create $kind with $secret in config [$broker]" || return 1
            assert_secret_rejection "$secret" "$sentinel" "create $kind, config object [$broker]" || return 1
            # create, secret as an undeclared top-level param
            kt_rejected "create-$tool_noun" "$(jq -nc --arg b "$broker" --arg n "$name" --arg np "$name_param" --arg s "$secret" --arg v "$sentinel" \
                '{broker:$b,msgVpnName:"default"} + {($np):$n} + {($s):$v}')" "create $kind with top-level $secret [$broker]" || return 1
            assert_secret_rejection "$secret" "$sentinel" "create $kind, top-level [$broker]" || return 1
            # update, secret inside the config object
            kt_rejected "update-$tool_noun" "$(jq -nc --arg b "$broker" --arg n "$name" --arg np "$name_param" --arg cp "$config_param" --arg s "$secret" --arg v "$sentinel" \
                '{broker:$b,msgVpnName:"default"} + {($np):$n} + {($cp):{($s):$v}}')" "update $kind with $secret [$broker]" || return 1
            assert_secret_rejection "$secret" "$sentinel" "update $kind, config object [$broker]" || return 1
            # update, secret as an undeclared top-level param
            kt_rejected "update-$tool_noun" "$(jq -nc --arg b "$broker" --arg n "$name" --arg np "$name_param" --arg s "$secret" --arg v "$sentinel" \
                '{broker:$b,msgVpnName:"default"} + {($np):$n} + {($s):$v}')" "update $kind with top-level $secret [$broker]" || return 1
            assert_secret_rejection "$secret" "$sentinel" "update $kind, top-level [$broker]" || return 1
        done
    done

    # Nothing was created, and the secret value is nowhere in the MCP server's log.
    local semp name_listed listing
    semp=$(semp_config_for "$broker")
    for name_listed in kafkaReceivers kafkaSenders; do
        listing=$(semp_curl -sf "$semp/msgVpns/$BROKER_VPN/$name_listed?count=100") \
            || { log_fail "secret rejection [$broker]: could not list $name_listed from SEMP"; return 1; }
        if [ "$(jq --arg p "e2e-config-kafka-rej-" '[.data[]? | (.kafkaReceiverName // .kafkaSenderName) | select(startswith($p))] | length > 0' <<<"$listing")" != "false" ]; then
            log_fail "secret rejection [$broker]: a rejected call created a $name_listed object"; return 1
        fi
    done
    assert_server_log_lacks "$sentinel" "secret rejection [$broker]" || return 1
}

# ── The enabled/scheme rules, and outcomes ───────────────────────────────────

test_kafka_enabled_rules() {
    local broker="$1"
    local rx="e2e-config-kafka-rules-$broker"
    local args
    args=$(jq -nc --arg b "$broker" --arg n "$rx" '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n}')

    kt_rejected "create-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"basic",enabled:true}}')" \
        "create enabled with a scheme [$broker]" || return 1
    assert_contains "$KT_TEXT" "enabled:false" "create enabled with a scheme [$broker]: tells the agent to create it disabled" || return 1

    kt_ok "create-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" --arg bs "$KAFKA_BOOTSTRAP_SASL" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"basic",bootstrapAddressList:$bs}}')" \
        "create disabled with a scheme [$broker]" || return 1

    kt_rejected "update-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"scram"}}')" \
        "update scheme without disabling [$broker]" || return 1
    assert_contains "$KT_TEXT" "enabled:false" "update scheme without disabling [$broker]: tells the agent to disable it too" || return 1

    kt_ok "update-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" \
        '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:{authenticationScheme:"scram",enabled:false}}')" \
        "update scheme and disable [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.summary.authenticationScheme' "scram" "update scheme [$broker]: scheme changed" || return 1
    # The switch also hands the user the per-scheme secret attributes to set.
    assert_json_field "$KT_CONTENT" '.summary.secretFields | index("authenticationScramPassword") != null' "true" \
        "update scheme [$broker]: the hand-off names the new scheme's password attribute" || return 1

    # An existing bridge cannot be re-pointed: the broker would present its stored
    # credentials to the new address.
    local cfg
    for cfg in '{"bootstrapAddressList":"elsewhere:9092"}' '{"authenticationOauthClientTokenEndpoint":"https://elsewhere/token"}' '{"transportTlsEnabled":false}'; do
        kt_rejected "update-kafka-receiver" "$(jq -nc --arg b "$broker" --arg n "$rx" --argjson c "$cfg" \
            '{broker:$b,msgVpnName:"default",kafkaReceiverName:$n,kafkaReceiverConfig:$c}')" "retarget $cfg [$broker]" || return 1
        assert_contains "$KT_TEXT" "stored credentials" "retarget $cfg [$broker]: the rejection says why" || return 1
    done

    kt_ok "delete-kafka-receiver" "$args" "delete [$broker]" || return 1
    # Deleting what is already gone is a noop, not an error.
    kt_ok "delete-kafka-receiver" "$args" "delete again [$broker]" || return 1
    assert_json_field "$KT_CONTENT" '.outcome' "already_absent" "delete again [$broker]: already_absent" || return 1
}

# ── Annotations ──────────────────────────────────────────────────────────────

test_kafka_annotations() {
    local sid resp t
    sid=$(mcp_initialize) || return 1
    resp=$(mcp_request "$sid" '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}') || return 1
    local all=(create-kafka-receiver update-kafka-receiver delete-kafka-receiver create-kafka-receiver-topic-binding delete-kafka-receiver-topic-binding
               create-kafka-sender update-kafka-sender delete-kafka-sender create-kafka-sender-queue-binding delete-kafka-sender-queue-binding)
    for t in "${all[@]}"; do
        assert_json_field "$resp" "([.result.tools[].name] | index(\"$t\")) != null" "true" "annotations: $t advertised in tools/list" || return 1
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.readOnlyHint) // false" "false" "annotations: $t readOnlyHint=false" || return 1
    done
    # create-kafka-receiver/sender are deliberately destructive (audited): the bridge
    # opens an outbound connection to a caller-chosen host. The binding creates keep
    # the create-* convention.
    for t in create-kafka-receiver create-kafka-sender update-kafka-receiver update-kafka-sender delete-kafka-receiver delete-kafka-sender \
             delete-kafka-receiver-topic-binding delete-kafka-sender-queue-binding; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "true" "annotations: $t destructiveHint=true" || return 1
    done
    for t in create-kafka-receiver-topic-binding create-kafka-sender-queue-binding; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "false" "annotations: $t destructiveHint=false" || return 1
    done
}

# ── Per-broker wrappers (run_test calls no-arg functions) ────────────────────
test_kafka_receiver_none_roundtrip_a() { test_kafka_receiver_none_roundtrip broker-a; }
test_kafka_receiver_none_roundtrip_b() { test_kafka_receiver_none_roundtrip broker-b; }
test_kafka_receiver_auth_roundtrip_a() { test_kafka_receiver_auth_roundtrip broker-a; }
test_kafka_receiver_auth_roundtrip_b() { test_kafka_receiver_auth_roundtrip broker-b; }
test_kafka_sender_roundtrip_a()        { test_kafka_sender_roundtrip broker-a; }
test_kafka_sender_roundtrip_b()        { test_kafka_sender_roundtrip broker-b; }
test_kafka_secret_rejection_a()        { test_kafka_secret_rejection broker-a; }
test_kafka_secret_rejection_b()        { test_kafka_secret_rejection broker-b; }
test_kafka_enabled_rules_a()           { test_kafka_enabled_rules broker-a; }
test_kafka_enabled_rules_b()           { test_kafka_enabled_rules broker-b; }

# ── Main ─────────────────────────────────────────────────────────────────────

run_test "Kafka annotations (tools/list)"                      test_kafka_annotations
run_test "Kafka secret rejection (broker-a)"                   test_kafka_secret_rejection_a
run_test "Kafka secret rejection (broker-b)"                   test_kafka_secret_rejection_b
run_test "Kafka enabled/scheme rules + outcomes (broker-a)"    test_kafka_enabled_rules_a
run_test "Kafka enabled/scheme rules + outcomes (broker-b)"    test_kafka_enabled_rules_b
run_test "Kafka Receiver, no authentication (broker-a)"        test_kafka_receiver_none_roundtrip_a
run_test "Kafka Receiver, no authentication (broker-b)"        test_kafka_receiver_none_roundtrip_b
run_test "Kafka Receiver, secret set out of band (broker-a)"   test_kafka_receiver_auth_roundtrip_a
run_test "Kafka Receiver, secret set out of band (broker-b)"   test_kafka_receiver_auth_roundtrip_b
run_test "Kafka Sender incl. missing topic (broker-a)"         test_kafka_sender_roundtrip_a
run_test "Kafka Sender incl. missing topic (broker-b)"         test_kafka_sender_roundtrip_b

print_summary "Kafka bridge write-tool tests"
