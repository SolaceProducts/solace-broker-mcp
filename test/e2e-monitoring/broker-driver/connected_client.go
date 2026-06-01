// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"solace.dev/go/messaging"
	"solace.dev/go/messaging/pkg/solace"
	"solace.dev/go/messaging/pkg/solace/config"
	"solace.dev/go/messaging/pkg/solace/message"
	"solace.dev/go/messaging/pkg/solace/resource"
)

// shutdownGrace is the time receivers/service get to stop cleanly before
// the process exits. Matches NFR-2 in SOL-150024 (5 s grace, then SIGKILL
// is the bash-level escalation; the Go side just gives up cleanly).
const shutdownGrace = 5 * time.Second

// runConnectedClient implements the F3 fixture: a long-lived connection that
// keeps a persistent-receiver bound to a queue AND a direct-receiver holding
// client-level topic subscriptions, so the broker's clients API reports both
// the deterministic clientName and the configured subscription set.
func runConnectedClient(args []string) int {
	fs := flag.NewFlagSet("connected-client", flag.ContinueOnError)
	broker := stringFlag(fs, "broker", "", "broker target: a or b (resolves to host:port from env)")
	vpn := stringFlag(fs, "vpn", "default", "message VPN to join")
	clientName := stringFlag(fs, "client-name", "", "deterministic clientName reported to the broker")
	queue := stringFlag(fs, "queue", "", "durable non-exclusive queue to bind the persistent receiver to")
	subs := stringFlag(fs, "subscriptions", "", "comma-separated client-level topic subscriptions (>= 1)")
	pidfile := stringFlag(fs, "pidfile", "", "path to write this process's PID to on startup")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *broker == "" || *clientName == "" || *queue == "" || *subs == "" || *pidfile == "" {
		fs.Usage()
		return fatalf("connected-client: --broker, --client-name, --queue, --subscriptions, --pidfile are all required")
	}

	host, ok := resolveBrokerHost(*broker)
	if !ok {
		return 1
	}

	subList := splitNonEmpty(*subs, ",")
	if len(subList) == 0 {
		return fatalf("connected-client: --subscriptions must list at least one topic")
	}

	service, err := buildMessagingService(host, *vpn, *clientName)
	if err != nil {
		return fatalf("build messaging service: %v", err)
	}
	if err := service.Connect(); err != nil {
		return fatalf("connect to broker %s: %v", host, err)
	}
	defer service.Disconnect()

	// Persistent receiver: queue-bound, no builder-level subscriptions so we
	// don't mutate the queue's subscription set. Just keeps the binding alive
	// for tools that inspect queue/client bindings.
	pReceiver, err := service.CreatePersistentMessageReceiverBuilder().
		Build(resource.QueueDurableNonExclusive(*queue))
	if err != nil {
		return fatalf("build persistent receiver on %q: %v", *queue, err)
	}
	if err := pReceiver.Start(); err != nil {
		return fatalf("start persistent receiver: %v", err)
	}
	if err := pReceiver.ReceiveAsync(noopMessageHandler); err != nil {
		return fatalf("register persistent receiver callback: %v", err)
	}
	defer pReceiver.Terminate(shutdownGrace)

	// Direct receiver: holds the client-level subscriptions that show up under
	// GET .../clients/<name>/subscriptions and are what list-client-subscriptions
	// reads. Drains messages with a no-op handler.
	dReceiver, err := service.CreateDirectMessageReceiverBuilder().
		WithSubscriptions(toSubscriptions(subList)...).
		Build()
	if err != nil {
		return fatalf("build direct receiver: %v", err)
	}
	if err := dReceiver.Start(); err != nil {
		return fatalf("start direct receiver: %v", err)
	}
	if err := dReceiver.ReceiveAsync(noopMessageHandler); err != nil {
		return fatalf("register direct receiver callback: %v", err)
	}
	defer dReceiver.Terminate(shutdownGrace)

	if err := writePidfile(*pidfile); err != nil {
		return fatalf("write pidfile %s: %v", *pidfile, err)
	}
	defer os.Remove(*pidfile)

	fmt.Fprintf(os.Stderr,
		"broker-driver connected-client ready: clientName=%s vpn=%s queue=%s subs=%v pid=%d host=%s\n",
		*clientName, *vpn, *queue, subList, os.Getpid(), host)

	waitForSignal()
	fmt.Fprintln(os.Stderr, "broker-driver connected-client: shutting down")
	return 0
}

// buildMessagingService configures a messaging service for a broker-driver
// subcommand. Uses the default VPN's pre-provisioned `default` client-username
// (no password), which is enabled out of the box on solace/solace-pubsub-standard.
func buildMessagingService(host, vpn, clientName string) (solace.MessagingService, error) {
	props := config.ServicePropertyMap{
		config.TransportLayerPropertyHost: host,
		config.ServicePropertyVPNName:     vpn,
		config.ClientPropertyName:         clientName,
	}
	return messaging.NewMessagingServiceBuilder().
		FromConfigurationProvider(props).
		WithAuthenticationStrategy(config.BasicUserNamePasswordAuthentication("default", "")).
		Build()
}

// resolveBrokerHost maps the --broker={a,b} flag to a SMF host:port using
// env vars sourced from test/e2e-monitoring/.env so broker-driver shares
// one source of truth with the bash harness.
func resolveBrokerHost(broker string) (string, bool) {
	var portEnv string
	switch broker {
	case "a":
		portEnv = "BROKER_A_SMF_PORT"
	case "b":
		portEnv = "BROKER_B_SMF_PORT"
	default:
		fmt.Fprintf(os.Stderr, "broker-driver: --broker must be 'a' or 'b' (got %q)\n", broker)
		return "", false
	}
	port, ok := requireEnv(portEnv)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("tcp://localhost:%s", port), true
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func toSubscriptions(topics []string) []resource.Subscription {
	subs := make([]resource.Subscription, 0, len(topics))
	for _, t := range topics {
		subs = append(subs, resource.TopicSubscriptionOf(t))
	}
	return subs
}

// writePidfile records the running PID so the bash harness can signal us via
// stop_broker_drivers. Mode 0644 matches the BROKER_DRIVER_PIDFILE_GLOB
// convention; the file is removed on clean exit via defer.
func writePidfile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}

// noopMessageHandler discards every received message. F3 only needs to
// establish the receiver state on the broker — message contents are
// irrelevant for the SEMP-visible assertions.
func noopMessageHandler(_ message.InboundMessage) {}
