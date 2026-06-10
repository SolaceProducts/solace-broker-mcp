package resilience

import (
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
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
