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
// them. /livez is the canonical liveness endpoint and returns
// {"status":"alive"}. /readyz is the canonical readiness endpoint and reflects
// the MCP server's OWN readiness only: it is built from ReadinessState
// (initialized flag, shutting-down flag, required-listener probes) via
// ReadyzHandler and is decoupled from broker reachability (ADR-004 /
// ISSUE-026) — it makes no broker calls and reads no broker state. /health is
// retained for backward compatibility and preserves its original
// {"status":"healthy"} body — it is NOT a body-identical alias of /livez and is
// served by its own handler (HealthHandler). The single accessor here,
// SaturationEventsEnabled, gates only the opt-in saturation-event signal
// layered on top of those probes. The v1 default for that signal is OFF
// (door-closing policy).
//
// There is deliberately NO generic Enabled accessor: the probes are
// unconditional, so a generic name would invite a future author to write
// `if health.Enabled(cfg) { registerLivez() }` and accidentally gate a probe
// that must always be served. Name the capability explicitly instead,
// mirroring metrics.AuthFailureCounterEnabled.
package health

import (
	"net/http"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// livenessBody is the exact response body served by the liveness probe. It is a
// process-alive signal only: a 200 means the HTTP server is accepting requests
// and the handler ran. It deliberately reports nothing about broker
// reachability — that is the readiness probe's job (/readyz).
const livenessBody = `{"status":"alive"}`

// healthBody is the exact response body served by the legacy /health endpoint.
// It preserves the originally shipped {"status":"healthy"} body so external
// consumers (dashboards, scripts) that parse .status == "healthy" keep working.
// /health is NOT a body-identical alias of /livez; the bodies differ
// deliberately. New consumers should use /livez (the canonical liveness
// endpoint, {"status":"alive"}).
const healthBody = `{"status":"healthy"}`

// LivezHandler returns the unconditional liveness handler. GET responds 200 with
// livenessBody ({"status":"alive"}); any other method responds 405. The probe is
// UNCONDITIONAL by design — there is no flag to disable it, so this constructor
// takes no config and is never gated (see the package doc).
//
// /livez is the canonical liveness endpoint. /health is a separate retained
// back-compat endpoint served by HealthHandler with its own ({"status":"healthy"})
// body — the two handlers are distinct instances and return different bodies.
func LivezHandler() http.Handler {
	return jsonProbeHandler(livenessBody)
}

// HealthHandler returns the legacy /health handler. GET responds 200 with
// healthBody ({"status":"healthy"}); any other method responds 405. It is
// retained for backward compatibility and preserves the original shipped body so
// existing consumers that assert .status == "healthy" do not break. /livez is the
// canonical liveness endpoint and returns {"status":"alive"} — HealthHandler is a
// distinct handler instance from LivezHandler, not a body-identical alias.
func HealthHandler() http.Handler {
	return jsonProbeHandler(healthBody)
}

// jsonProbeHandler builds an unconditional probe handler that responds 200 with
// the given JSON body on GET and 405 on any other method. Shared by LivezHandler
// and HealthHandler so the two probes stay behaviorally identical apart from
// their (deliberately different) bodies.
func jsonProbeHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
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
