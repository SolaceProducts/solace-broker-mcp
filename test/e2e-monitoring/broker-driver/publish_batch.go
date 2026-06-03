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
	"time"

	"solace.dev/go/messaging/pkg/solace/config"
	"solace.dev/go/messaging/pkg/solace/resource"
)

// runPublishBatch implements the F6 fixture role: a short-lived persistent
// publisher that sends exactly --count messages of --size bytes to --topic,
// then exits. Used twice by helpers.sh — once for F6-spool (fills the queue's
// spool quota so the broker discards messages and increments
// maxMsgSpoolUsageExceededDiscardedMsgCount) and once for F6-ttl (publishes
// with --dmq-eligible=false so TTL-expired messages are truly discarded and
// increment maxTtlExpiredDiscardedMsgCount rather than being moved to the DMQ).
// No PID file is written because the process is short-lived and the bash
// harness does not need to signal it.
func runPublishBatch(args []string) int {
	fs := flag.NewFlagSet("publish-batch", flag.ContinueOnError)
	broker := stringFlag(fs, "broker", "", "broker target: a or b (resolves to host:port from env)")
	vpn := stringFlag(fs, "vpn", "default", "message VPN to join")
	clientName := stringFlag(fs, "client-name", "", "clientName; defaults to e2e-monitoring-publish-batch-<broker>")
	topic := stringFlag(fs, "topic", "", "destination topic")
	count := fs.Int("count", 0, "number of messages to publish (required, > 0)")
	size := fs.Int("size", 256, "payload size in bytes")
	msgType := stringFlag(fs, "message-type", "persistent", "publish QoS: 'persistent' (only supported value today)")
	rate := fs.Int("rate", 0, "target messages per second (0 = as fast as possible)")
	dmqEligible := fs.Bool("dmq-eligible", true, "mark messages as DMQ-eligible (set false for F6-ttl so expired messages hit maxTtlExpiredDiscardedMsgCount)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *broker == "" || *topic == "" {
		fs.Usage()
		return fatalf("publish-batch: --broker and --topic are required")
	}
	if *count <= 0 {
		return fatalf("publish-batch: --count must be > 0 (got %d)", *count)
	}
	if *size <= 0 {
		return fatalf("publish-batch: --size must be > 0 (got %d)", *size)
	}
	if *msgType != "persistent" {
		return fatalf("publish-batch: --message-type=%q not supported (only 'persistent')", *msgType)
	}
	if *rate < 0 {
		return fatalf("publish-batch: --rate must be >= 0 (got %d)", *rate)
	}

	host, ok := resolveBrokerHost(*broker)
	if !ok {
		return 1
	}

	resolvedClientName := *clientName
	if resolvedClientName == "" {
		resolvedClientName = fmt.Sprintf("e2e-monitoring-publish-batch-%s", *broker)
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
	defer publisher.Terminate(batchFlushGrace)

	payload := bytes.Repeat([]byte{'x'}, *size)
	dest := resource.TopicOf(*topic)

	// Build the per-message publish function once. For F6-ttl (dmqEligible=false)
	// we use the message builder to mark each message as not DMQ-eligible; when
	// the broker TTL-expires such a message it increments
	// maxTtlExpiredDiscardedMsgCount rather than attempting a DMQ move. For
	// F6-spool (dmqEligible=true, the default) PublishBytes is sufficient.
	var publishFn func() error
	if *dmqEligible {
		publishFn = func() error {
			return publisher.PublishBytes(payload, dest)
		}
	} else {
		msgBuilder := service.MessageBuilder().
			WithProperty(config.MessagePropertyPersistentDMQEligible, false)
		publishFn = func() error {
			msg, err := msgBuilder.BuildWithByteArrayPayload(payload)
			if err != nil {
				return err
			}
			return publisher.Publish(msg, dest, nil, nil)
		}
	}

	fmt.Fprintf(os.Stderr,
		"broker-driver publish-batch: topic=%s count=%d size=%dB type=%s rate=%d dmq-eligible=%v host=%s\n",
		*topic, *count, *size, *msgType, *rate, *dmqEligible, host)

	sent, failed := publishBatch(publishFn, *count, *rate)
	fmt.Fprintf(os.Stderr, "broker-driver publish-batch: done sent=%d failed=%d\n", sent, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// publishBatch calls publish exactly count times, pacing via a ticker when
// rate > 0. Returns (sent, failed) counts. failed counts only local publish
// errors — broker-level discards (spool-quota exceeded, TTL expiry) arrive
// asynchronously as NACKs and are not tracked here; the verification step
// asserts the discard counters via SEMP rather than via publish receipts.
func publishBatch(publish func() error, count, rate int) (int64, int64) {
	var sent, failed int64

	if rate <= 0 {
		for i := 0; i < count; i++ {
			if err := publish(); err != nil {
				failed++
				continue
			}
			sent++
		}
		return sent, failed
	}

	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	for i := 0; i < count; i++ {
		<-ticker.C
		if err := publish(); err != nil {
			failed++
			continue
		}
		sent++
	}
	return sent, failed
}
