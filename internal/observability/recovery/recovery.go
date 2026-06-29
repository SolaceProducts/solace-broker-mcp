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

// Package recovery provides panic recovery for the request path. It exposes
// the Enabled capability gate (OBS_PANIC_RECOVERY_ENABLED) and HTTPMiddleware,
// the whole-mux recover() wrapper that traps a panicking HTTP handler and
// converts it to a clean 500 (SOL-151286). The v1 default is ON (door-closing
// policy). The tool-handler goroutine is recovered separately by withRecovery
// in internal/tools, gated by the same flag (SOL-151287).
//
// The package is named recovery (not recover) so it does not shadow the Go
// builtin recover(), which HTTPMiddleware uses.
package recovery

import "github.com/SolaceDev/solace-broker-mcp/internal/config"

// Enabled reports whether panic recovery is turned on, reading the
// OBS_PANIC_RECOVERY_ENABLED flag off the observability config. main() consults
// it before installing HTTPMiddleware and before gating the tool-path wrapper.
func Enabled(cfg config.ObservabilityConfig) bool {
	return cfg.PanicRecoveryEnabled
}
