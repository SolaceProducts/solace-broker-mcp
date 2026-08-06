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
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// Semaphore bounds in-flight SEMP requests for a single broker. One instance
// is created per broker (see semp.NewBrokerClient) and shared by that broker's
// SEMPv1 and SEMPv2 Senders, so the cap applies to the broker as a whole
// rather than per protocol client.
//
// A buffered channel rather than golang.org/x/sync/semaphore.Weighted:
// len() exposes the current in-flight count, which the SOL-149791
// observability work needs for the broker-pool gauge.
type Semaphore chan struct{}

// NewSemaphore creates a Semaphore admitting at most n concurrent holders.
// Config validation enforces 1..MaxConcurrentPerBrokerCeiling, so n < 1 only
// occurs for hand-built configs (tests); it falls back to
// DefaultMaxConcurrentPerBroker rather than deadlocking on an unbuffered
// channel.
func NewSemaphore(n int) Semaphore {
	if n < 1 {
		n = defaults.DefaultMaxConcurrentPerBroker
	}
	return make(Semaphore, n)
}
