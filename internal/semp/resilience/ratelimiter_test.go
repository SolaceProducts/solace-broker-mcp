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

import (
	"testing"
	"time"
)

// A non-positive interval must disable throttling outright. Sender.Do receives
// from the limiter's channel on every request, so anything other than a
// never-blocking channel here would stall every unthrottled deployment.
func TestNewRateLimiter_DisabledNeverBlocks(t *testing.T) {
	for _, interval := range []time.Duration{0, -1 * time.Second} {
		limiter := NewRateLimiter(interval)
		for i := range 3 {
			select {
			case <-limiter.C():
			default:
				t.Fatalf("interval %v: receive %d blocked; a disabled limiter must never block", interval, i)
			}
		}
	}
}

// A configured interval must actually pace: the first receive waits, since a
// ticker does not fire immediately.
func TestNewRateLimiter_PositiveIntervalPaces(t *testing.T) {
	const interval = 50 * time.Millisecond
	limiter := NewRateLimiter(interval)
	defer limiter.Stop()

	select {
	case <-limiter.C():
		t.Fatal("limiter admitted immediately; a configured interval must pace the first request")
	default:
	}

	start := time.Now()
	<-limiter.C()
	if elapsed := time.Since(start); elapsed < interval/2 {
		t.Errorf("first admission took %v, want at least ~%v", elapsed, interval)
	}
}

// Stop is documented as safe on a disabled limiter and safe to call more than
// once, because BrokerClient.Close() is its single stop site but callers may
// close a broker client repeatedly (e.g. a deferred Close plus pool teardown).
func TestRateLimiter_StopIsSafeAndIdempotent(t *testing.T) {
	disabled := NewRateLimiter(0)
	disabled.Stop()
	disabled.Stop()

	enabled := NewRateLimiter(10 * time.Millisecond)
	enabled.Stop()
	enabled.Stop()
}
