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

package sempv2_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// truncatedBodyHandler answers 200 — the broker has acted on the request — and
// drops the connection partway through the body, so the client fails while
// reading the response rather than inside the Sender.
func truncatedBodyHandler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"data":{"queueName":`)
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

// SOL-155411: a write whose response is lost after the broker answered may
// already be applied, so it must carry the same outcome-unknown flag the
// Sender sets for a write that failed after it was sent.
func TestClient_Execute_WriteResponseBodyLost_MarksOutcomeUnknown(t *testing.T) {
	client, server := newTestClient(t, truncatedBodyHandler)
	defer server.Close()

	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			op := &sempv2.Operation{ID: "writeOp", Method: method, Path: "/SEMP/v2/config/msgVpns/default/queues"}
			_, err := client.Execute(context.Background(), op, map[string]any{})

			var exhausted *resilience.RetriesExhaustedError
			if !errors.As(err, &exhausted) || !exhausted.NonIdempotent {
				t.Fatalf("want a NonIdempotent *RetriesExhaustedError for a %s whose response was lost, got %T: %v",
					method, err, err)
			}
		})
	}
}

// Control: a read whose response is lost changed nothing, so it keeps the
// ordinary error and stays retryable.
func TestClient_Execute_ReadResponseBodyLost_NotMarkedOutcomeUnknown(t *testing.T) {
	client, server := newTestClient(t, truncatedBodyHandler)
	defer server.Close()

	op := &sempv2.Operation{ID: "readOp", Method: http.MethodGet, Path: "/SEMP/v2/monitor/msgVpns/default/queues"}
	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected a body-read error, got nil")
	}
	var exhausted *resilience.RetriesExhaustedError
	if errors.As(err, &exhausted) {
		t.Errorf("GET body-read failure wrapped as *RetriesExhaustedError (NonIdempotent=%v); a read must stay retryable",
			exhausted.NonIdempotent)
	}
}
