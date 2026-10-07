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
	"fmt"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests pin the operator's side of SOL-155412: what the log's detail field
// holds for a request body the executor rejected locally. They share the helpers
// in request_body_errors_test.go.

// P4: the operator sees the real reason in the log's detail field, not the Go
// type name. error_type stays execution_error: reclassifying it is out of scope.
func TestRequestBodyRejection_LogDetailCarriesMessage(t *testing.T) {
	out := callRealWriteTool(t, "update-topic-endpoint", map[string]any{
		"msgVpnName":          "default",
		"topicEndpointName":   "te1",
		"topicEndpointConfig": map[string]any{"maxMsgSpoolUsage": 5},
	})

	detail, _ := out.log["detail"].(string)
	if strings.HasPrefix(detail, "*") {
		t.Errorf("detail = %q, want the message text and not a Go type name", detail)
	}
	if !strings.Contains(detail, "is not a known attribute") || !strings.Contains(detail, `"maxMsgSpoolUsage"`) {
		t.Errorf("detail = %q, want it to carry the executor's message", detail)
	}
	if strings.Contains(detail, "executing tool") || strings.Contains(detail, "tool step") {
		t.Errorf("detail = %q, want the inner message only, without the wrapper prefixes", detail)
	}
	// The operator reads the same sentence the agent got, with nothing added.
	if detail != out.reply {
		t.Errorf("detail = %q, want exactly the reply the agent received, %q", detail, out.reply)
	}
	if got := out.log["error_type"]; got != "execution_error" {
		t.Errorf("error_type = %v, want %q (reclassification is out of scope)", got, "execution_error")
	}
	if got := out.log["tool"]; got != "update-topic-endpoint" {
		t.Errorf("tool = %v, want %q", got, "update-topic-endpoint")
	}
}

// P5 (danger), part ii: a broker server error wrapped by an executor step must
// not turn into verbatim text. This goes red if the matcher for the local
// rejection ever also catches a broker error. Its detail for broker types is
// unchanged, so only the reply is asserted.
func TestRequestBodyRejection_BrokerServerErrorInStep_StaysGeneric(t *testing.T) {
	brokerErr := &sempv2.SEMPError{
		Operation:   "updateMsgVpnQueue",
		StatusCode:  500,
		Description: "internal broker detail that must not reach the agent",
		SEMPCode:    500,
		SEMPStatus:  "FAIL",
	}
	result, _ := callStubError(t, fmt.Errorf("tool step updateQueue: %w", brokerErr))

	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if text != genericInternalMessage {
		t.Errorf("reply = %q, want the generic message for a broker 500", text)
	}
	if strings.Contains(text, "internal broker detail") {
		t.Errorf("reply leaked the broker's text: %q", text)
	}
}
