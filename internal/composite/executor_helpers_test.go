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

package composite

import (
	"context"
	"sync"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// mockClient implements sempv2.Client for testing. It records which operations
// were called (in order) and returns preconfigured responses.
type mockClient struct {
	mu        sync.Mutex
	calls     []string
	responses map[string]*sempv2.Result
	errors    map[string]error
}

func newMockClient() *mockClient {
	return &mockClient{
		responses: make(map[string]*sempv2.Result),
		errors:    make(map[string]error),
	}
}

func (m *mockClient) Execute(_ context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, op.ID)
	m.mu.Unlock()

	if err, ok := m.errors[op.ID]; ok {
		return nil, err
	}

	if resp, ok := m.responses[op.ID]; ok {
		return resp, nil
	}

	return &sempv2.Result{
		Data:       map[string]any{"status": "ok"},
		StatusCode: 200,
	}, nil
}

// testOperations returns a minimal operation catalog for tests.
func testOperations() map[string]*sempv2.Operation {
	return map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueue": {
			ID:     "getMsgVpnQueue",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}",
		},
		"monitor/getMsgVpnClient": {
			ID:     "getMsgVpnClient",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}",
		},
		"monitor/getMsgVpnClientSubscriptions": {
			ID:     "getMsgVpnClientSubscriptions",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}/subscriptions",
		},
		"action/doMsgVpnQueueCancelReplay": {
			ID:     "doMsgVpnQueueCancelReplay",
			Method: "PUT",
			Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/cancelReplay",
		},
		"action/doMsgVpnQueueStartReplay": {
			ID:     "doMsgVpnQueueStartReplay",
			Method: "PUT",
			Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/startReplay",
		},
		"monitor/getMsgVpn": {
			ID:     "getMsgVpn",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}",
		},
		"monitor/getMsgVpns": {
			ID:     "getMsgVpns",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns",
		},
		"monitor/getMsgVpnQueues": {
			ID:     "getMsgVpnQueues",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/queues",
		},
		"monitor/getMsgVpnClients": {
			ID:     "getMsgVpnClients",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/clients",
		},
		"monitor/getMsgVpnRestDeliveryPoints": {
			ID:     "getMsgVpnRestDeliveryPoints",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/restDeliveryPoints",
		},
		"config/createMsgVpn": {
			ID:     "createMsgVpn",
			Method: "POST",
			Path:   "/SEMP/v2/__private_config__/msgVpns",
			Parameters: []sempv2.Parameter{
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/updateMsgVpn": {
			ID:     "updateMsgVpn",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/deleteMsgVpn": {
			ID:     "deleteMsgVpn",
			Method: "DELETE",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}",
		},
		"config/createMsgVpnQueue": {
			ID:     "createMsgVpnQueue",
			Method: "POST",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/queues",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/updateMsgVpnQueue": {
			ID:     "updateMsgVpnQueue",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/queues/{queueName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "queueName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/deleteMsgVpnQueue": {
			ID:     "deleteMsgVpnQueue",
			Method: "DELETE",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/queues/{queueName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "queueName", In: "path", Type: "string", Required: true},
			},
		},
		"config/createMsgVpnTopicEndpoint": {
			ID:     "createMsgVpnTopicEndpoint",
			Method: "POST",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/topicEndpoints",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/updateMsgVpnTopicEndpoint": {
			ID:     "updateMsgVpnTopicEndpoint",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/topicEndpoints/{topicEndpointName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "topicEndpointName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/deleteMsgVpnTopicEndpoint": {
			ID:     "deleteMsgVpnTopicEndpoint",
			Method: "DELETE",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/topicEndpoints/{topicEndpointName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "topicEndpointName", In: "path", Type: "string", Required: true},
			},
		},
		"config/createMsgVpnRestDeliveryPoint": {
			ID:     "createMsgVpnRestDeliveryPoint",
			Method: "POST",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/restDeliveryPoints",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/updateMsgVpnRestDeliveryPoint": {
			ID:     "updateMsgVpnRestDeliveryPoint",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/restDeliveryPoints/{restDeliveryPointName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "restDeliveryPointName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/deleteMsgVpnRestDeliveryPoint": {
			ID:     "deleteMsgVpnRestDeliveryPoint",
			Method: "DELETE",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/restDeliveryPoints/{restDeliveryPointName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "restDeliveryPointName", In: "path", Type: "string", Required: true},
			},
		},
	}
}

// seqMockClient returns pre-configured responses for operations in sequence.
// Each Execute call for an operation advances to the next response in its sequence.
// When the sequence is exhausted the last response is repeated.
type seqMockClient struct {
	mu     sync.Mutex
	calls  []callRecord
	seqs   map[string][]*sempv2.Result
	errors map[string]error
	idx    map[string]int
}

func newSeqMockClient() *seqMockClient {
	return &seqMockClient{
		seqs:   make(map[string][]*sempv2.Result),
		errors: make(map[string]error),
		idx:    make(map[string]int),
	}
}

func (m *seqMockClient) addResponses(opID string, results ...*sempv2.Result) {
	m.seqs[opID] = append(m.seqs[opID], results...)
}

func (m *seqMockClient) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, callRecord{opID: op.ID, args: args})

	if err, ok := m.errors[op.ID]; ok {
		return nil, err
	}

	seq, ok := m.seqs[op.ID]
	if !ok || len(seq) == 0 {
		return &sempv2.Result{Data: map[string]any{}, StatusCode: 200}, nil
	}

	idx := m.idx[op.ID]
	if idx >= len(seq) {
		idx = len(seq) - 1
	}
	m.idx[op.ID] = idx + 1
	return seq[idx], nil
}

// argCapturingClient wraps a client and records the args passed to each Execute call.
type argCapturingClient struct {
	inner    sempv2.Client
	recorded *[]callRecord
	mu       *sync.Mutex
}

type callRecord struct {
	opID string
	args map[string]any
}

func (c *argCapturingClient) Execute(ctx context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	c.mu.Lock()
	*c.recorded = append(*c.recorded, callRecord{opID: op.ID, args: args})
	c.mu.Unlock()
	return c.inner.Execute(ctx, op, args)
}

// pageResult builds a SEMP list response with optional pagination cursor.
func pageResult(items []any, nextCursor string) *sempv2.Result {
	data := map[string]any{"data": items}
	if nextCursor != "" {
		data["meta"] = map[string]any{
			"paging": map[string]any{
				"nextPageUri": "/SEMP/v2/__private_monitor__/resource?cursor=" + nextCursor + "&count=100",
			},
		}
	}
	return &sempv2.Result{Data: data, StatusCode: 200}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
