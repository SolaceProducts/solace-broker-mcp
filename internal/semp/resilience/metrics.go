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

package resilience

import (
	"net/http"
	"strconv"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// metricsTransport wraps an http.RoundTripper to record one SEMP sample per
// attempt. retryablehttp calls the transport once per try, so this is the one
// place that sees every attempt. Installed only when metrics are on.
type metricsTransport struct {
	base        http.RoundTripper
	recorder    *metrics.SEMPMetrics
	api         string
	brokerAlias string
}

// RoundTrip times one try and records its method, host, status, and attempt
// number. A try that gets no response records an empty status. Redirect hops
// (req.Response != nil) are passed through without recording — they are not
// new SEMP attempts.
func (t *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil {
		return t.base.RoundTrip(req)
	}
	attempt := nextAttempt(req.Context())

	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	dur := time.Since(start)

	status := ""
	if resp != nil {
		status = strconv.Itoa(resp.StatusCode)
	}

	// req.Context(), never a fresh one: it carries the caller's `semp.request`
	// span, and the metrics SDK reads that span off the observation context to
	// attach the trace exemplar a slow bucket links to (SOL-152419). A bare
	// context here still records a correct histogram and silently emits no
	// exemplar, with no error on any surface —
	// TestExecute_LatencyBucketCarriesTheRequestSpansTraceID (internal/semp/sempv2)
	// is what catches it, including across a retried attempt.
	t.recorder.Record(req.Context(), metrics.SEMPRequest{
		API:       t.api,
		Broker:    t.brokerAlias,
		Operation: operationID(req.Context()),
		Method:    req.Method,
		Status:    status,
		Address:   req.URL.Hostname(),
		Attempt:   attempt,
	}, dur)

	return resp, err
}
