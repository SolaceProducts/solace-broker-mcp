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

// Package correlation is the home for correlation-ID propagation. Skeleton
// (SOL-151278): only the capability gate exists today; the middleware that
// stamps and propagates a correlation ID lands in a later story. The v1
// default is ON (door-closing policy).
package correlation

import "github.com/SolaceProducts/solace-broker-mcp/internal/config"

// Enabled reports whether correlation-ID propagation is turned on, reading the
// OBS_CORRELATION_ID_ENABLED flag off the observability config. Later wiring
// consults this before installing the correlation middleware.
func Enabled(cfg config.ObservabilityConfig) bool {
	return cfg.CorrelationIDEnabled
}
