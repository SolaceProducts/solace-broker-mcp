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

package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// BrokerOccupancy is one broker's in-flight-semaphore occupancy at a moment in
// time. Limit is the configured semp.max_concurrent_per_broker cap, carried
// alongside the count because the count alone is unreadable: "8 in flight" is
// healthy against a cap of 50 and saturated against a cap of 8.
type BrokerOccupancy struct {
	Broker   string // configured broker alias, as it appears in the config file
	InFlight int
	Limit    int
}

// OccupancySnapshot returns current occupancy for every broker that has been
// realized. Implemented by semp.BrokerPool, which creates broker clients lazily
// — an unused broker has no semaphore and correctly appears in no snapshot.
//
// Called from the reporter's goroutine, so the implementation must be safe for
// concurrent use.
type OccupancySnapshot func() []BrokerOccupancy

// occupancyMessage is the single log message for this signal. Constant so an
// operator can grep one string, and so the tests assert against the same
// literal the reporter emits.
const occupancyMessage = "broker in-flight occupancy"

// StartOccupancyReporter periodically logs how full each broker's in-flight
// semaphore is, and returns a stop function that halts it.
//
// This is the interim, log-based half of SOL-153443. The observability schema
// specifies this as a gauge on a Prometheus /metrics endpoint, but this build
// has no such endpoint, no meter provider and no OpenTelemetry dependency, and
// standing one up here would duplicate the work in flight under SOL-150254. A
// periodic log line needs none of that and answers the same question: which
// broker is under pressure right now. See docs/observability.md.
//
// Gated on OBS_SATURATION_EVENTS_ENABLED. Disabled, it starts no goroutine at
// all rather than starting one that does nothing, so the off state costs
// nothing and the snapshot function is never called.
//
// Only brokers carrying load are reported. Logging every configured broker
// every interval would bury the signal in its own noise, and the presence of a
// line is itself the information that a broker is busy. A broker at its cap
// logs at WARN rather than INFO: that is the state where every further request
// queues at the concurrency gate, and it is what pairs with the per-request
// "broker admission slow" warning from the Sender.
//
// stop is idempotent and waits for the reporting goroutine to exit, so a caller
// that stops the reporter before draining logs sees no line afterwards.
func StartOccupancyReporter(cfg config.ObservabilityConfig, interval time.Duration, snapshot OccupancySnapshot) (stop func()) {
	// A nil snapshot is a wiring mistake. Treat it as "nothing to report"
	// rather than panicking on a background goroutine, where the crash would
	// surface far from the line that caused it. Same for a non-positive
	// interval, which would make time.NewTicker panic.
	if !SaturationEventsEnabled(cfg) || snapshot == nil || interval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	var exited sync.WaitGroup
	exited.Add(1)

	go func() {
		defer exited.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Report once before waiting out the first interval. An operator who
		// turns this on part-way through an incident should not have to wait a
		// full interval — 60s by default — for the first reading.
		reportOccupancy(snapshot())
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				reportOccupancy(snapshot())
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		exited.Wait()
	}
}

// reportOccupancy emits one line per busy broker. Split out so the decision of
// what is worth logging is testable without a ticker.
func reportOccupancy(occupancies []BrokerOccupancy) {
	for _, occ := range occupancies {
		if occ.InFlight <= 0 {
			continue
		}
		level := slog.LevelInfo
		if occ.Limit > 0 && occ.InFlight >= occ.Limit {
			level = slog.LevelWarn
		}
		// Background rather than a request context: this line is emitted on a
		// timer tick and belongs to no request, so there is no correlation ID
		// for the handler to pick up.
		//
		// Broker alias and two integers, all server-side config or counters —
		// nothing here is broker- or caller-sourced text.
		slog.Log(context.Background(), level, occupancyMessage,
			slog.String("broker", occ.Broker),
			slog.Int("in_flight", occ.InFlight),
			slog.Int("limit", occ.Limit))
	}
}
