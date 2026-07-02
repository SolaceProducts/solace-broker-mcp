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
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStampCorrelationID_Unit exercises stampCorrelationID's full input domain
// directly: nil result, empty ctx ID, nil-Meta init, and the
// preserve-existing-entries guarantee.
func TestStampCorrelationID_Unit(t *testing.T) {
	t.Run("nil result is a no-op (no panic)", func(t *testing.T) {
		stampCorrelationID(correlation.With(context.Background(), "id"), nil)
	})

	t.Run("empty ctx ID adds nothing and leaves Meta nil", func(t *testing.T) {
		res := &mcp.CallToolResult{}
		stampCorrelationID(context.Background(), res)
		if res.Meta != nil {
			t.Errorf("Meta = %#v, want nil when ctx has no correlation ID", res.Meta)
		}
	})

	t.Run("nil Meta is initialized and stamped", func(t *testing.T) {
		res := &mcp.CallToolResult{}
		stampCorrelationID(correlation.With(context.Background(), "the-id"), res)
		if got := res.Meta[metaKeyCorrelationID]; got != "the-id" {
			t.Errorf("Meta[%q] = %v, want %q", metaKeyCorrelationID, got, "the-id")
		}
	})

	t.Run("existing Meta entries are preserved", func(t *testing.T) {
		res := &mcp.CallToolResult{Meta: mcp.Meta{"keep": "value"}}
		stampCorrelationID(correlation.With(context.Background(), "the-id"), res)
		if got := res.Meta["keep"]; got != "value" {
			t.Errorf("pre-existing Meta[\"keep\"] = %v, want it preserved as %q", got, "value")
		}
		if got := res.Meta[metaKeyCorrelationID]; got != "the-id" {
			t.Errorf("Meta[%q] = %v, want %q", metaKeyCorrelationID, got, "the-id")
		}
	})
}
