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

package auth

import (
	"reflect"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

func TestAdvertisedPRM_ConfiguredSurfaces(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		resourceURL     string
		wantMetadataURL string
		wantPaths       []string
		wantHandler     bool
	}{
		{"oauth mcp path", config.AuthModeOAuth, "https://mcp.example.com/mcp", "https://mcp.example.com/.well-known/oauth-protected-resource", []string{prmBarePath, prmBarePath + "/mcp"}, true},
		{"oauth ingress path", config.AuthModeOAuth, "https://mcp.example.com/broker/mcp", "https://mcp.example.com/.well-known/oauth-protected-resource", []string{prmBarePath, prmBarePath + "/broker/mcp"}, true},
		{"oauth empty path", config.AuthModeOAuth, "https://mcp.example.com", "https://mcp.example.com/.well-known/oauth-protected-resource", []string{prmBarePath}, true},
		{"oauth root path", config.AuthModeOAuth, "https://mcp.example.com/", "https://mcp.example.com/.well-known/oauth-protected-resource", []string{prmBarePath}, true},
		{"static URL only", config.AuthModeStatic, "http://localhost:9090/mcp", "http://localhost:9090/.well-known/oauth-protected-resource", nil, false},
		{"disabled none", config.AuthModeDisabled, "http://localhost:9090/mcp", "", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{
				Mode: tt.mode, ResourceURL: tt.resourceURL, Issuer: "https://auth.example.com",
			}}
			prm := NewAdvertisedPRM(cfg)

			if got := prm.ResourceMetadataURL(); got != tt.wantMetadataURL {
				t.Errorf("ResourceMetadataURL() = %q, want %q", got, tt.wantMetadataURL)
			}
			if got := prm.Paths(); !reflect.DeepEqual(got, tt.wantPaths) {
				t.Errorf("Paths() = %v, want %v", got, tt.wantPaths)
			}
			if got := prm.Handler() != nil; got != tt.wantHandler {
				t.Errorf("Handler present = %v, want %v", got, tt.wantHandler)
			}
		})
	}
}
