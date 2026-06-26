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

import (
	"net/http"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// livenessBody is the exact response body served by the liveness probe. It is a
// process-alive signal only: a 200 means the HTTP server is accepting requests
// and the handler ran. It deliberately reports nothing about broker
// reachability — that is the readiness probe's job (/readyz).
const livenessBody = `{"status":"alive"}`

// LivezHandler returns the unconditional liveness handler. GET responds 200 with
// livenessBody; any other method responds 405. The probe is UNCONDITIONAL by
// design — there is no flag to disable it, so this constructor takes no config
// and is never gated (see the package doc).
//
// /health registers this same handler as a backward-compatible alias, so
// /health and /livez return byte-identical responses and identical 405 behavior.
func LivezHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(livenessBody)); err != nil {
			http.Error(w, "failed to write response", http.StatusInternalServerError)
		}
	})
}

// SaturationEventsEnabled reports whether the opt-in saturation-event signal is
// turned on, reading the OBS_SATURATION_EVENTS_ENABLED flag off the
// observability config. It does NOT gate the liveness (/livez) or readiness
// (/readyz) probes, which are unconditional. Later wiring consults this before
// starting the saturation detector.
func SaturationEventsEnabled(cfg config.ObservabilityConfig) bool {
	return cfg.SaturationEventsEnabled
}
