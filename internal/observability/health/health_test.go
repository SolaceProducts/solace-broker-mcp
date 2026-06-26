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
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// TestSaturationEventsEnabled pins that the accessor reflects the
// SaturationEventsEnabled flag rather than a hardcoded constant — both
// directions. There is deliberately no generic Enabled accessor: the
// liveness/readiness probes are unconditional and must never be gated.
func TestSaturationEventsEnabled(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		cfg := &config.ServerConfig{}
		cfg.Observability.SaturationEventsEnabled = want
		if got := SaturationEventsEnabled(cfg); got != want {
			t.Errorf("SaturationEventsEnabled() = %v, want %v", got, want)
		}
	}
}
