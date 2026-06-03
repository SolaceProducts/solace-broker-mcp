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
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"solace.dev/go/messaging/pkg/solace"
	"solace.dev/go/messaging/pkg/solace/message"
	"solace.dev/go/messaging/pkg/solace/resource"
)

// receivePollTimeout bounds each blocking ReceiveMessage call so the consume
// loop checks the shutdown channel at least this often. Short relative to the
// 5 s shutdown grace so SIGTERM is honoured promptly even between messages.
const receivePollTimeout = time.Second

// runSlowConsumer implements the F5 fixture: a single process that drives a
// slow *guaranteed-message* consumer. It fast-publishes persistent messages
// into a dedicated queue (via the queue's subscribed topic) while a
// queue-bound receiver in client-acknowledgement mode ACKs each message only
// after --ack-delay. With the queue's maxDeliveredUnackedMsgsPerFlow set low,
// the broker delivers up to that window then stalls, so txUnackedMsgCount pins
// near the ceiling while the publisher keeps spooling: spooledMsgCount grows
// and rxMsgRate outpaces txMsgRate. These are the queue-level signals
// verify-fixtures.sh asserts (SOL-150344) — the per-client slowSubscriber flag
// never flips for this case (SOL-150328).
func runSlowConsumer(args []string) int {
	fs := flag.NewFlagSet("slow-consumer", flag.ContinueOnError)
	broker := stringFlag(fs, "broker", "", "broker target: a or b (resolves to host:port from env)")
	vpn := stringFlag(fs, "vpn", "default", "message VPN to join")
	clientName := stringFlag(fs, "client-name", "", "deterministic clientName; defaults to e2e-monitoring-slow-consumer-<broker>")
	queue := stringFlag(fs, "queue", "", "durable non-exclusive queue to bind the slow receiver to")
	topic := stringFlag(fs, "topic", "", "topic the queue subscribes to; the fast publisher targets it")
	rate := fs.Int("rate", 100, "target publish rate in messages per second")
	size := fs.Int("size", 256, "payload size in bytes")
	ackDelay := fs.Duration("ack-delay", 2*time.Second, "delay before ACKing each received message (the throttle)")
	pidfile := stringFlag(fs, "pidfile", "", "path to write this process's PID to on startup")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *broker == "" || *queue == "" || *topic == "" || *pidfile == "" {
		fs.Usage()
		return fatalf("slow-consumer: --broker, --queue, --topic, --pidfile are all required")
	}
	if *rate <= 0 {
		return fatalf("slow-consumer: --rate must be > 0 (got %d)", *rate)
	}
	if *size <= 0 {
		return fatalf("slow-consumer: --size must be > 0 (got %d)", *size)
	}
	if *ackDelay < 0 {
		return fatalf("slow-consumer: --ack-delay must be >= 0 (got %s)", *ackDelay)
	}

	host, ok := resolveBrokerHost(*broker)
	if !ok {
		return 1
	}

	resolvedClientName := *clientName
	if resolvedClientName == "" {
		resolvedClientName = fmt.Sprintf("e2e-monitoring-slow-consumer-%s", *broker)
	}
	service, err := buildMessagingService(host, *vpn, resolvedClientName)
	if err != nil {
		return fatalf("build messaging service: %v", err)
	}
	if err := service.Connect(); err != nil {
		return fatalf("connect to broker %s: %v", host, err)
	}
	defer service.Disconnect()

	// Fast publisher into the queue's topic.
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

	// Slow receiver: client-acknowledgement so unacked messages count against
	// the queue's per-flow window until we explicitly Ack them.
	receiver, err := service.CreatePersistentMessageReceiverBuilder().
		WithMessageClientAcknowledgement().
		Build(resource.QueueDurableNonExclusive(*queue))
	if err != nil {
		return fatalf("build persistent receiver on %q: %v", *queue, err)
	}
	if err := receiver.Start(); err != nil {
		return fatalf("start persistent receiver: %v", err)
	}
	defer receiver.Terminate(terminateGrace)

	if err := writePidfile(*pidfile); err != nil {
		return fatalf("write pidfile %s: %v", *pidfile, err)
	}
	defer os.Remove(*pidfile)

	fmt.Fprintf(os.Stderr,
		"broker-driver slow-consumer ready: queue=%s topic=%s rate=%d msg/s size=%dB ack-delay=%s pid=%d host=%s\n",
		*queue, *topic, *rate, *size, *ackDelay, os.Getpid(), host)

	// stop is closed (not sent to) on signal so both the publisher goroutine
	// and the consume loop observe shutdown — a closed channel unblocks every
	// reader, whereas a signal delivery would reach only one of them. Its
	// element type is os.Signal only to satisfy publishLoop's signature (F4
	// uses that channel as a real signal carrier for --duration); here no value
	// is ever sent, so the type is incidental — treat stop as a done channel.
	stop := make(chan os.Signal)
	go func() {
		sig := signalChannel()
		<-sig
		close(stop)
	}()

	// Run the publisher in the background so it floods the topic while the
	// foreground consume loop throttles. Join it before returning so the
	// deferred publisher.Terminate does not race a still-running PublishBytes
	// (F4's runPublisher gets this for free by publishing synchronously).
	var publisherDone sync.WaitGroup
	publisherDone.Go(func() {
		publishLoop(publisher, *topic, *size, *rate, stop)
	})
	slowConsumeLoop(receiver, *ackDelay, stop)
	publisherDone.Wait()

	fmt.Fprintln(os.Stderr, "broker-driver slow-consumer: shutting down")
	return 0
}

// slowConsumeLoop receives messages synchronously and ACKs each only after
// ackDelay, throttling the consumer. Both the inter-message receive (bounded by
// receivePollTimeout) and the ackDelay wait observe stop, so a closed stop
// channel is honoured promptly — even mid-delay — rather than blocking shutdown
// until the delay elapses. Receive timeouts are expected (the publisher may
// briefly fall behind) and are ignored.
func slowConsumeLoop(receiver persistentAckReceiver, ackDelay time.Duration, stop <-chan os.Signal) {
	var acked int64
consume:
	for {
		select {
		case <-stop:
			break consume
		default:
		}

		msg, err := receiver.ReceiveMessage(receivePollTimeout)
		if err != nil {
			var timeout *solace.TimeoutError
			if errors.As(err, &timeout) {
				continue // no message this interval; re-check stop and retry
			}
			fmt.Fprintf(os.Stderr, "slow-consumer receive: %v\n", err)
			continue
		}

		// Throttle: wait ackDelay before ACKing, but abort promptly if stop
		// fires mid-delay. The message is left unacked and redelivered when a
		// receiver next binds — fine for a fixture that's shutting down.
		timer := time.NewTimer(ackDelay)
		select {
		case <-stop:
			timer.Stop()
			break consume
		case <-timer.C:
		}

		if err := receiver.Ack(msg); err != nil {
			fmt.Fprintf(os.Stderr, "slow-consumer ack: %v\n", err)
			continue
		}
		acked++
	}
	fmt.Fprintf(os.Stderr, "slow-consumer loop: acked=%d\n", acked)
}

// persistentAckReceiver narrows the Solace receiver surface the consume loop
// uses, mirroring persistentBytesPublisher in publisher.go.
type persistentAckReceiver interface {
	ReceiveMessage(timeout time.Duration) (message.InboundMessage, error)
	Ack(msg message.InboundMessage) error
}
