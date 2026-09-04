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

package tools

import (
	"context"
	"net/http"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
)

// Metric broker-label sentinels. The log line and the user-facing error keep the
// raw alias the caller typed (a diagnostic); the metric label is canonicalized
// to a bounded set so a typo cannot mint unbounded series.
const (
	// brokerLabelNone is used when there is no broker: a brokerless tool
	// (list-brokers, describe-semp-schema) or a failure before resolution
	// (bad_request, missing_broker).
	brokerLabelNone = "none"
	// brokerLabelUnknown is used when the caller named a broker that is not
	// configured, so it has no canonical form.
	brokerLabelUnknown = "unknown"
)

// canonicalBrokerLabel maps an alias to a bounded metric label: the configured
// display name, or a sentinel when the alias is empty or unconfigured.
func canonicalBrokerLabel(pool *semp.BrokerPool, alias string) string {
	if alias == "" {
		return brokerLabelNone
	}
	if bc, ok := pool.BrokerConfig(alias); ok {
		return bc.DisplayName()
	}
	return brokerLabelUnknown
}

// recordToolInvocation records the RED metrics for one invocation on tm
// (nil-safe). Kept separate from logToolResult so the metric broker label
// (canonical) can differ from the logged one (raw).
func recordToolInvocation(ctx context.Context, tm *metrics.ToolMetrics, tool, metricBroker string, start time.Time, errorType metrics.ErrorType, toolErr error) {
	outcome := metrics.OutcomeSuccess
	et := metrics.ErrorType("")
	if toolErr != nil {
		outcome = metrics.OutcomeError
		et = errorType
	}
	tm.Record(ctx, tool, metricBroker, outcome, et, time.Since(start))
}

// ActiveRequestsMiddleware increments the in-flight request gauge on entry and
// decrements it on exit via a deferred call, so a panicking handler still
// decrements. No-op when tm is nil (metrics disabled).
func ActiveRequestsMiddleware(tm *metrics.ToolMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tm.IncActive(r.Context())
		defer tm.DecActive(r.Context())
		next.ServeHTTP(w, r)
	})
}
