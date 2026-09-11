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
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

func TestNewTokenExchanger_LogsUnconfiguredExpiryFallback(t *testing.T) {
	// NOT parallel: this test swaps the process-global logger.
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	cfg := &config.BrokerOAuthConfig{
		TokenURL: "https://idp.example.com/token",
		ClientID: "mcp-client",
		ClientAuth: config.BrokerClientAuth{
			ClientSecretBasic: &config.ClientSecretAuth{Secret: "test-secret"},
		},
		GrantType:     config.GrantTypeTokenExchange,
		AudienceParam: config.AudienceParamAudience,
		// TokenExpiryFallback intentionally nil: omission is the default path.
	}

	exchanger, err := newTokenExchanger(cfg)
	if err != nil {
		t.Fatalf("newTokenExchanger: %v", err)
	}
	t.Cleanup(func() {
		if err := exchanger.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	out := buf.String()
	if !strings.Contains(out, `"msg":"token exchanger created for broker OAuth"`) {
		t.Fatalf("startup log not captured: %s", out)
	}
	if !strings.Contains(out, `"expiry_fallback_configured":false`) {
		t.Errorf("expected expiry_fallback_configured=false: %s", out)
	}
	if strings.Contains(out, `"expiry_fallback":`) {
		t.Errorf("expiry_fallback must be absent when unconfigured: %s", out)
	}
}
