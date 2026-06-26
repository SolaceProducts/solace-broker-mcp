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

// Package tracing is the home for distributed tracing. Skeleton
// (SOL-151278): only the capability gate exists today; the OpenTelemetry
// tracer setup and span instrumentation land in a later story. The v1 default
// is OFF (door-closing policy) — operators opt in.
package tracing

import "github.com/SolaceDev/solace-broker-mcp/internal/config"

// Enabled reports whether tracing is turned on, reading the OBS_TRACING_ENABLED
// flag off the loaded config. Later wiring consults this before configuring the
// tracer provider.
func Enabled(cfg *config.ServerConfig) bool {
	return cfg.Observability.TracingEnabled
}
