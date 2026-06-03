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

	"solace.dev/go/messaging/pkg/solace/resource"
)

// runSlowDirectSubscriber implements the F6 fixture: a long-lived DIRECT
// receiver holding a single topic subscription and nothing else. It exists so
// the broker's per-client slowSubscriber flag can be flipped to true.
//
// The flag tracks TCP egress back-pressure — the broker has data to send but
// the client's receive window has stayed closed for several seconds in the last
// minute. A slow application callback is NOT enough to trigger it: the Solace
// client's underlying C library drains the OS socket on its own thread
// regardless of how slow the app is, and direct receivers only offer DROP
// back-pressure. To actually close the TCP window the harness SIGSTOPs this
// whole process while a separate publisher floods the subscribed topic with
// large payloads (see helpers.sh create_slow_subscriber_on). This process
// therefore deliberately does NOT publish — bundling the flood here would mean
// SIGSTOP halts the flood too, and the window would never fill.
//
// Background: the F5 slow-consumer fixture (slow_consumer.go) drives a slow
// guaranteed-message consumer, which surfaces at the queue level and never
// flips slowSubscriber (SOL-150328). F6 is the per-client-flag counterpart
// that list-slow-subscribers needs.
func runSlowDirectSubscriber(args []string) int {
	fs := flag.NewFlagSet("slow-direct-subscriber", flag.ContinueOnError)
	broker := stringFlag(fs, "broker", "", "broker target: a or b (resolves to host:port from env)")
	vpn := stringFlag(fs, "vpn", "default", "message VPN to join")
	clientName := stringFlag(fs, "client-name", "", "deterministic clientName; defaults to e2e-monitoring-slow-subscriber-<broker>")
	topic := stringFlag(fs, "topic", "", "topic to subscribe to; the separate flood publisher targets it")
	pidfile := stringFlag(fs, "pidfile", "", "path to write this process's PID to on startup")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *broker == "" || *topic == "" || *pidfile == "" {
		fs.Usage()
		return fatalf("slow-direct-subscriber: --broker, --topic, --pidfile are all required")
	}

	host, ok := resolveBrokerHost(*broker)
	if !ok {
		return 1
	}

	resolvedClientName := *clientName
	if resolvedClientName == "" {
		resolvedClientName = fmt.Sprintf("e2e-monitoring-slow-subscriber-%s", *broker)
	}
	service, err := buildMessagingService(host, *vpn, resolvedClientName)
	if err != nil {
		return fatalf("build messaging service: %v", err)
	}
	if err := service.Connect(); err != nil {
		return fatalf("connect to broker %s: %v", host, err)
	}
	defer service.Disconnect()

	// Direct receiver holding one topic subscription. A no-op handler drains
	// messages app-side; the SIGSTOP-induced TCP stall (not the handler) is what
	// closes the egress window and flips slowSubscriber.
	receiver, err := service.CreateDirectMessageReceiverBuilder().
		WithSubscriptions(resource.TopicSubscriptionOf(*topic)).
		Build()
	if err != nil {
		return fatalf("build direct receiver: %v", err)
	}
	if err := receiver.Start(); err != nil {
		return fatalf("start direct receiver: %v", err)
	}
	if err := receiver.ReceiveAsync(noopMessageHandler); err != nil {
		return fatalf("register direct receiver callback: %v", err)
	}
	defer receiver.Terminate(terminateGrace)

	if err := writePidfile(*pidfile); err != nil {
		return fatalf("write pidfile %s: %v", *pidfile, err)
	}
	defer os.Remove(*pidfile)

	fmt.Fprintf(os.Stderr,
		"broker-driver slow-direct-subscriber ready: clientName=%s vpn=%s topic=%s pid=%d host=%s\n",
		resolvedClientName, *vpn, *topic, os.Getpid(), host)

	waitForSignal()
	fmt.Fprintln(os.Stderr, "broker-driver slow-direct-subscriber: shutting down")
	return 0
}
