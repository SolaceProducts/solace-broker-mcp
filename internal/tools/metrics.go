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
	"sync/atomic"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
)

// Metric broker-label sentinels. The log line always shows the raw alias the
// caller typed (a diagnostic); the metric label is canonicalized to a bounded
// set so a caller cannot mint unbounded series with typos.
const (
	// brokerLabelNone is used when there is no broker: a brokerless tool
	// (list-brokers, describe-semp-schema) or a failure before resolution
	// (bad_request, missing_broker).
	brokerLabelNone = "none"
	// brokerLabelUnknown is used when the caller named a broker that is not
	// configured, so it has no canonical form.
	brokerLabelUnknown = "unknown"
)

// toolMetrics holds the RED-metric recorder. Nil until metrics are enabled, so
// the disabled path (the default) costs one atomic load and no allocation.
var toolMetrics atomic.Pointer[metrics.ToolMetrics]

// SetToolMetrics installs the recorder at startup. Passing nil is legal and
// disables recording — every ToolMetrics method is nil-receiver-safe.
func SetToolMetrics(tm *metrics.ToolMetrics) {
	toolMetrics.Store(tm)
}

// canonicalBrokerLabel maps a caller-supplied alias to a bounded metric label:
// the configured display name (case-insensitive match, the same lookup the
// success path uses), or a fixed sentinel when the alias is empty or not
// configured. Only the metric label is canonicalized; the log line and the
// user-facing error keep the raw alias so a caller can see their own typo.
func canonicalBrokerLabel(pool *semp.BrokerPool, alias string) string {
	if alias == "" {
		return brokerLabelNone
	}
	if bc, ok := pool.BrokerConfig(alias); ok {
		return bc.DisplayName()
	}
	return brokerLabelUnknown
}

// recordToolInvocation records the RED metrics for one tool invocation. It is a
// sibling to logToolResult, kept separate so the metric label (metricBroker,
// canonical and bounded) can differ from the logged broker (raw). outcome is
// derived from toolErr; error_type is attached only on the error path.
func recordToolInvocation(ctx context.Context, tool, metricBroker string, start time.Time, errorType string, toolErr error) {
	outcome := "success"
	et := ""
	if toolErr != nil {
		outcome = "error"
		et = errorType
	}
	toolMetrics.Load().Record(ctx, tool, metricBroker, outcome, et, time.Since(start))
}

// ActiveRequestsMiddleware increments the in-flight request gauge on entry and
// decrements it on exit via a deferred call, so a panicking handler still
// decrements. No-op when metrics are disabled (nil recorder).
func ActiveRequestsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tm := toolMetrics.Load()
		tm.IncActive(r.Context())
		defer tm.DecActive(r.Context())
		next.ServeHTTP(w, r)
	})
}
