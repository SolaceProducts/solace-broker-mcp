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
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"solace.dev/go/messaging/pkg/solace/resource"
)

// publisherBackpressureBuffer is the in-flight message capacity for the
// OnBackPressureWait strategy. 1000 holds ~10 s of buffered messages at the
// F4 target rate of 100 msg/s, so brief broker stalls don't cause the loop
// to block long enough to perturb the steady-state rate the AC measures.
const publisherBackpressureBuffer = 1000

// runPublisher implements the F4 (sustained traffic) fixture role: a
// long-lived persistent publisher that publishes a fixed-size byte payload
// to a topic at a target rate until it receives SIGINT/SIGTERM or hits
// --duration.
func runPublisher(args []string) int {
	fs := flag.NewFlagSet("publisher", flag.ContinueOnError)
	broker := stringFlag(fs, "broker", "", "broker target: a or b (resolves to host:port from env)")
	vpn := stringFlag(fs, "vpn", "default", "message VPN to join")
	clientName := stringFlag(fs, "client-name", "", "deterministic clientName; defaults to e2e-monitoring-publisher-<broker>")
	topic := stringFlag(fs, "topic", "", "destination topic (must be subscribed by a receiver for AC 5 txMsgRate to tick)")
	rate := fs.Int("rate", 100, "target messages per second")
	size := fs.Int("size", 256, "payload size in bytes")
	msgType := stringFlag(fs, "message-type", "persistent", "publish QoS: 'persistent' (only supported value today)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = run until signal)")
	pidfile := stringFlag(fs, "pidfile", "", "path to write this process's PID to on startup")
	priority := fs.Int("priority", -1, "message priority 0-255 (unset = fast PublishBytes path). Values below the endpoint's reject-low-priority-msg-limit trip lowPriorityMsgCongestionState on spool-limited queues.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *broker == "" || *topic == "" || *pidfile == "" {
		fs.Usage()
		return fatalf("publisher: --broker, --topic, --pidfile are all required")
	}
	if *rate <= 0 {
		return fatalf("publisher: --rate must be > 0 (got %d)", *rate)
	}
	if *size <= 0 {
		return fatalf("publisher: --size must be > 0 (got %d)", *size)
	}
	if *msgType != "persistent" {
		return fatalf("publisher: --message-type=%q not supported (only 'persistent')", *msgType)
	}
	if *priority < -1 || *priority > 255 {
		return fatalf("publisher: --priority must be -1 (unset) or 0..255 (got %d)", *priority)
	}

	host, ok := resolveBrokerHost(*broker)
	if !ok {
		return 1
	}

	resolvedClientName := *clientName
	if resolvedClientName == "" {
		resolvedClientName = fmt.Sprintf("e2e-monitoring-publisher-%s", *broker)
	}
	service, err := buildMessagingService(host, *vpn, resolvedClientName)
	if err != nil {
		return fatalf("build messaging service: %v", err)
	}
	if err := service.Connect(); err != nil {
		return fatalf("connect to broker %s: %v", host, err)
	}
	defer service.Disconnect()

	publisher, err := service.CreatePersistentMessagePublisherBuilder().
		OnBackPressureWait(publisherBackpressureBuffer).
		Build()
	if err != nil {
		return fatalf("build persistent publisher: %v", err)
	}
	if err := publisher.Start(); err != nil {
		return fatalf("start persistent publisher: %v", err)
	}
	defer publisher.Terminate(terminateGrace)

	if err := writePidfile(*pidfile); err != nil {
		return fatalf("write pidfile %s: %v", *pidfile, err)
	}
	defer os.Remove(*pidfile)

	payload := bytes.Repeat([]byte{'x'}, *size)
	dest := resource.TopicOf(*topic)
	publishFn := func() error {
		return publisher.PublishBytes(payload, dest)
	}
	if *priority >= 0 {
		msgBuilder := service.MessageBuilder().WithPriority(*priority)
		publishFn = func() error {
			msg, err := msgBuilder.BuildWithByteArrayPayload(payload)
			if err != nil {
				return err
			}
			return publisher.Publish(msg, dest, nil, nil)
		}
	}

	fmt.Fprintf(os.Stderr,
		"broker-driver publisher ready: topic=%s rate=%d msg/s size=%dB type=%s priority=%d pid=%d host=%s\n",
		*topic, *rate, *size, *msgType, *priority, os.Getpid(), host)

	done := signalChannel()
	if *duration > 0 {
		go func() {
			time.Sleep(*duration)
			select {
			case done <- syscall.SIGTERM:
			default:
			}
		}()
	}

	sent, failed := publishLoop(publishFn, *rate, done)
	fmt.Fprintf(os.Stderr, "broker-driver publisher: shutting down sent=%d failed=%d\n", sent, failed)
	// Mirror publish-batch: a non-zero failed count is a real fault (with
	// OnBackPressureWait, PublishBytes blocks on backpressure rather than
	// erroring), so don't let a publisher whose every send failed exit 0.
	if failed > 0 {
		return 1
	}
	return 0
}

// publishLoop fires once per tick at `rate` msg/s until the done channel
// signals, returning the (sent, failed) publish counts. PersistentMessage
// Publisher.PublishBytes blocks only when the in-flight buffer is full
// (OnBackPressureWait); for the F4 target rate against an idle broker the
// buffer never fills, so each tick publishes promptly. The ~8% steady-state
// undershoot the spec acknowledges comes from the Go scheduler + broker ack
// roundtrip, not from this loop.
func publishLoop(publish func() error, rate int, done <-chan os.Signal) (int64, int64) {
	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var sent, failed int64
	for {
		select {
		case <-done:
			return sent, failed
		case <-ticker.C:
			if err := publish(); err != nil {
				failed++
				continue
			}
			sent++
		}
	}
}

// signalChannel returns a buffered chan that delivers SIGINT/SIGTERM.
// Buffered so the duration-based goroutine can drop into the same channel
// without blocking when no signal handler has fired.
func signalChannel() chan os.Signal {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}
