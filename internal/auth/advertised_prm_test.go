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
	"bytes"
	"encoding/json"
	"log/slog"
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

func TestAdvertisedPRM_Log(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantLine bool
	}{
		{"oauth", config.AuthModeOAuth, true},
		{"static", config.AuthModeStatic, false},
		{"disabled", config.AuthModeDisabled, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			defer slog.SetDefault(previous)

			cfg := &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{
				Mode:        tt.mode,
				ResourceURL: "https://resource-user:resource-pass@mcp.example.com/mcp",
				Issuer:      "https://issuer-user:issuer-pass@auth.example.com/realm",
			}}
			NewAdvertisedPRM(cfg).Log()

			if !tt.wantLine {
				if buf.Len() != 0 {
					t.Fatalf("Log() emitted outside OAuth mode: %s", buf.String())
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("decode log: %v", err)
			}
			if got["msg"] != "registered OAuth protected resource metadata endpoint" {
				t.Errorf("msg = %v", got["msg"])
			}
			if got["level"] != "INFO" {
				t.Errorf("level = %v, want INFO", got["level"])
			}
			want := map[string]any{
				"resource":                 "https://mcp.example.com/mcp",
				"issuers":                  []any{"https://auth.example.com/realm"},
				"scopes_supported":         []any{"openid"},
				"bearer_methods_supported": []any{"header"},
				"resource_metadata_url":    "https://mcp.example.com/.well-known/oauth-protected-resource",
				"prm_paths":                []any{prmBarePath, prmBarePath + "/mcp"},
			}
			for key, wantValue := range want {
				if !reflect.DeepEqual(got[key], wantValue) {
					t.Errorf("%s = %#v, want %#v", key, got[key], wantValue)
				}
			}
		})
	}
}
