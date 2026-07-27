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

package resilience

import "time"

// RateLimiter paces outbound SEMP requests for a single broker. One instance is
// created per broker (see semp.NewBrokerClient) and shared by that broker's
// SEMPv1 and SEMPv2 Senders, so the interval applies to the broker as a whole
// rather than per protocol client.
//
// Sharing is the whole point of the type. A Sender-private limiter silently
// admits up to 2x the configured rate, because each protocol client paces only
// itself: a ticker channel buffers one tick, so an idle protocol always has a
// tick waiting and its next request is admitted with no spacing at all. That is
// the same defect class the Semaphore contract guards against for the in-flight
// cap (SOL-150116), and it is why the Sender constructor, resilience.New,
// panics on a nil limiter.
type RateLimiter struct {
	c      <-chan time.Time
	ticker *time.Ticker // nil when throttling is disabled
}

// NewRateLimiter creates a limiter admitting at most one request per interval.
//
// A request arriving within one interval of construction waits for the first
// tick; one that arrives later finds a tick already buffered and is admitted
// immediately. The ticker runs regardless of demand and its channel holds one
// tick, so idle time is effectively credit. This is a deliberate trade: it
// bounds the sustained rate, which is what request_min_interval is for, without
// the complexity of a seeded channel and a forwarding goroutine. It does not
// bound a burst after an idle period, and a single buffered tick caps that
// burst at one request.
//
// The same buffering is why the limiter must be shared rather than per-Sender:
// see the type comment above.
//
// A non-positive interval disables throttling: the limiter yields a closed
// channel, so receives never block.
func NewRateLimiter(interval time.Duration) *RateLimiter {
	if interval <= 0 {
		ch := make(chan time.Time)
		close(ch)
		return &RateLimiter{c: ch}
	}
	ticker := time.NewTicker(interval)
	return &RateLimiter{c: ticker.C, ticker: ticker}
}

// C returns the channel a Sender receives from before admitting a request.
func (r *RateLimiter) C() <-chan time.Time { return r.c }

// Stop releases the underlying ticker. Safe on a limiter with throttling
// disabled, and safe to call more than once. Owned by BrokerClient.Close(),
// which is the single stop site for the broker's limiter.
func (r *RateLimiter) Stop() {
	if r.ticker != nil {
		r.ticker.Stop()
	}
}
