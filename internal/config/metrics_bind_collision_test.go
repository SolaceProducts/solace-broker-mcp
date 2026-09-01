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

package config

import (
	"strings"
	"testing"
)

// A metrics_bind_address sharing the MCP server's port is refused at config
// load. The guard is wildcard-aware ("", "0.0.0.0", "::" overlap any host).
func TestValidate_MetricsBindAddressCollision(t *testing.T) {
	tests := []struct {
		name           string
		port           string // top-level `port:` (empty => default 9090)
		listenAddress  string // `listen_address:` (empty => omitted => default 127.0.0.1)
		metricsBind    string // observability.metrics_bind_address (empty => omitted => default :9091)
		metricsEnabled bool
		wantErr        bool
	}{
		{
			name:           "defaults do not collide (MCP 9090 vs metrics 9091)",
			metricsEnabled: true,
			wantErr:        false,
		},
		{
			name:           "same port, wildcard metrics overlaps loopback MCP",
			metricsBind:    ":9090", // wildcard host, MCP defaults to 127.0.0.1:9090
			metricsEnabled: true,
			wantErr:        true,
		},
		{
			name:           "same port, both explicit wildcard",
			listenAddress:  "0.0.0.0",
			port:           "9091",
			metricsBind:    "0.0.0.0:9091",
			metricsEnabled: true,
			wantErr:        true,
		},
		{
			name:           "same port, localhost vs 127.0.0.1 are the same interface",
			listenAddress:  "localhost",
			metricsBind:    "127.0.0.1:9090",
			metricsEnabled: true,
			wantErr:        true,
		},
		{
			name:           "same port but distinct specific hosts do not collide",
			listenAddress:  "127.0.0.1",
			metricsBind:    "192.0.2.10:9090", // TEST-NET-1, a routable-looking but reserved host
			metricsEnabled: true,
			wantErr:        false,
		},
		{
			name:           "same port collision is ignored when metrics are disabled",
			metricsBind:    ":9090",
			metricsEnabled: false,
			wantErr:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearObsEnv(t)
			if tc.metricsEnabled {
				t.Setenv("OBS_METRICS_ENABLED", "true")
			}

			var b strings.Builder
			b.WriteString("mcp_client_auth:\n  mode: static\n  dev_token: test\n")
			if tc.port != "" {
				b.WriteString("port: " + tc.port + "\n")
			}
			if tc.listenAddress != "" {
				b.WriteString("listen_address: " + tc.listenAddress + "\n")
			}
			if tc.metricsBind != "" {
				b.WriteString("observability:\n  metrics_bind_address: \"" + tc.metricsBind + "\"\n")
			}
			b.WriteString("brokers:\n  prod:\n    url: \"https://broker.example.com:1943\"\n    auth:\n      mode: basic\n      username: admin\n      password: secret\n")

			_, err := LoadConfig(writeTemp(t, b.String()))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a collision error, got nil")
				}
				if !strings.Contains(err.Error(), "metrics_bind_address") {
					t.Errorf("error should name metrics_bind_address, got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}
