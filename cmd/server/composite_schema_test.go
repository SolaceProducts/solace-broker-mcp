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

package main

import (
	"encoding/json"
	"testing"
)

// TestWriteToolObjectNamesHaveMinLength pins the SOL-151811 guardrail on the
// real embedded tools.yaml, through the full registration pipeline: every write
// tool's object-name params carry minLength:1, and the optional *Config bodies
// do not. This puts the boundary in a test rather than in prose, where it can
// silently drift — the guardrail was already lost once (SOL-151646) without a
// failing test. It also covers the four create-* tools, whose object name is a
// body field the earlier path-param derivation missed entirely.
func TestWriteToolObjectNamesHaveMinLength(t *testing.T) {
	wantMinLength := map[string][]string{
		"create-message-vpn":    {"msgVpnName"},
		"update-message-vpn":    {"msgVpnName"},
		"delete-message-vpn":    {"msgVpnName"},
		"create-queue":          {"msgVpnName", "queueName"},
		"update-queue":          {"msgVpnName", "queueName"},
		"delete-queue":          {"msgVpnName", "queueName"},
		"delete-queue-messages": {"msgVpnName", "queueName"},
		"clear-queue-stats":     {"msgVpnName", "queueName"},
		"create-topic-endpoint": {"msgVpnName", "topicEndpointName"},
		"update-topic-endpoint": {"msgVpnName", "topicEndpointName"},
		"delete-topic-endpoint": {"msgVpnName", "topicEndpointName"},
		"create-rdp":            {"msgVpnName", "restDeliveryPointName"},
		"update-rdp":            {"msgVpnName", "restDeliveryPointName"},
		"delete-rdp":            {"msgVpnName", "restDeliveryPointName"},
		"disconnect-client":     {"msgVpnName", "clientName"},
		"clear-client-stats":    {"msgVpnName", "clientName"},
	}
	// Optional *Config bodies must stay unconstrained.
	wantNoMinLength := map[string]string{
		"create-message-vpn": "msgVpnConfig",
		"create-queue":       "queueConfig",
		"create-rdp":         "rdpConfig",
	}

	server := registeredServer(t, true)
	schemas := map[string]map[string]property{}
	for _, tool := range listAllTools(t, server) {
		schemas[tool.Name] = properties(t, tool.InputSchema)
	}

	for name, params := range wantMinLength {
		props, ok := schemas[name]
		if !ok {
			t.Errorf("write tool %q not registered", name)
			continue
		}
		for _, p := range params {
			prop, ok := props[p]
			if !ok {
				t.Errorf("%s.%s not in schema", name, p)
				continue
			}
			if prop.MinLength == nil || *prop.MinLength != 1 {
				t.Errorf("%s.%s minLength = %v, want 1", name, p, prop.MinLength)
			}
		}
	}

	for name, param := range wantNoMinLength {
		props, ok := schemas[name]
		if !ok {
			t.Errorf("write tool %q not registered", name)
			continue
		}
		prop, ok := props[param]
		if !ok {
			t.Errorf("%s.%s not in schema", name, param)
			continue
		}
		if prop.MinLength != nil {
			t.Errorf("%s.%s minLength = %d, want unset", name, param, *prop.MinLength)
		}
	}
}

type property struct {
	Type      string `json:"type"`
	MinLength *int   `json:"minLength"`
}

// properties re-marshals a tool's input schema (typed any after the tools/list
// round-trip) into a param-name → property map.
func properties(t *testing.T, inputSchema any) map[string]property {
	t.Helper()
	raw, err := json.Marshal(inputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]property `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal input schema: %v", err)
	}
	return schema.Properties
}
