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

// Package health is the home for the server's saturation / health-signal
// capability. Skeleton (SOL-151278): only the capability gate exists today;
// the saturation detector and its event emission land in a later story.
//
// Scope note: liveness (/livez) and readiness (/readyz) are UNCONDITIONAL by
// design — there is no flag to disable them, and this package does NOT gate
// them. (/health is retained as an alias for /livez for backward
// compatibility; /livez and /readyz are the canonical names the next story
// implements.) The single accessor here, SaturationEventsEnabled, gates only
// the opt-in saturation-event signal layered on top of those probes. The v1
// default for that signal is OFF (door-closing policy).
//
// There is deliberately NO generic Enabled accessor: the probes are
// unconditional, so a generic name would invite a future author to write
// `if health.Enabled(cfg) { registerLivez() }` and accidentally gate a probe
// that must always be served. Name the capability explicitly instead,
// mirroring metrics.AuthFailureCounterEnabled.
package health

import "github.com/SolaceDev/solace-broker-mcp/internal/config"

// SaturationEventsEnabled reports whether the opt-in saturation-event signal is
// turned on, reading the OBS_SATURATION_EVENTS_ENABLED flag off the loaded
// config. It does NOT gate the liveness (/livez) or readiness (/readyz) probes,
// which are unconditional. Later wiring consults this before starting the
// saturation detector.
func SaturationEventsEnabled(cfg *config.ServerConfig) bool {
	return cfg.Observability.SaturationEventsEnabled
}
