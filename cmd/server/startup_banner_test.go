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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// TestLogStartupBanners_NoCredentialLeak drives the same helper main() uses to
// emit boot-time banners (logStartupBanners), fed a real *config.ServerConfig
// loaded from YAML that carries sentinel credentials in every secret-holding
// field reachable from the helper: MCPClientAuth.DevToken plus one basic and
// one bearer broker's auth fields. If any future edit to logStartupBanners
// pipes one of these fields into a banner call, the sentinel substring turns
// the leak into a deterministic test failure. SOL-150757.
func TestLogStartupBanners_NoCredentialLeak(t *testing.T) {
	const (
		sentDevTok    = "SENTINEL_DEV_TOKEN_MUST_NOT_LEAK"
		sentBasicUser = "SENTINEL_BASIC_USERNAME_MUST_NOT_LEAK"
		sentBasicPass = "SENTINEL_BASIC_PASSWORD_MUST_NOT_LEAK"
		sentBearerTok = "SENTINEL_BEARER_TOKEN_MUST_NOT_LEAK"
	)
	sentinels := []string{sentDevTok, sentBasicUser, sentBasicPass, sentBearerTok}

	// Cover each mode main() may see. Bind/TLS shape is chosen per mode to
	// exercise the follow-up cleartext-exposure and plaintext-listener banners
	// so all three logStartupBanners emission paths run.
	cases := []struct {
		name           string
		clientAuthYAML string
		listenAddress  string
		tlsExtras      string
	}{
		{
			// disabled: only LogStartupAuthMode fires.
			name: "disabled",
			clientAuthYAML: fmt.Sprintf(`mcp_client_auth:
  mode: disabled
  dev_token: %s
`, sentDevTok),
			listenAddress: "127.0.0.1",
		},
		{
			// static + non-loopback + no TLS -> also fires LogStaticCleartextExposure.
			name: "static_cleartext",
			clientAuthYAML: fmt.Sprintf(`mcp_client_auth:
  mode: static
  dev_token: %s
`, sentDevTok),
			listenAddress: "0.0.0.0",
		},
		{
			// oauth + tls_terminated_upstream -> also fires LogOAuthPlaintextListener.
			name: "oauth_plaintext_upstream",
			clientAuthYAML: fmt.Sprintf(`mcp_client_auth:
  mode: oauth
  issuer: https://idp.example.com
  audience: mcp
  resource_url: https://mcp.example.com/mcp
  dev_token: %s
  tool_authorization:
    enabled: false
`, sentDevTok),
			listenAddress: "0.0.0.0",
			tlsExtras:     "tls_terminated_upstream: true\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`port: 9090
listen_address: %s
%s%sbrokers:
  basic-broker:
    url: https://broker-a.example.com:1943
    auth:
      mode: basic
      username: %s
      password: %s
  bearer-broker:
    url: https://broker-b.example.com:1943
    auth:
      mode: bearer
      token: %s
`, tc.listenAddress, tc.tlsExtras, tc.clientAuthYAML, sentBasicUser, sentBasicPass, sentBearerTok)

			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.LoadConfig(cfgPath)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			// Prove the sentinels made it into cfg — otherwise the leak
			// assertion below is vacuous (this is the bug the previous
			// version of this test had).
			assertContains := func(label, got, want string) {
				t.Helper()
				if !strings.Contains(got, want) {
					t.Fatalf("%s did not carry sentinel: got %q, want to contain %q", label, got, want)
				}
			}
			assertContains("MCPClientAuth.DevToken", cfg.MCPClientAuth.DevToken, sentDevTok)
			basic, _ := cfg.Broker("basic-broker")
			bearer, _ := cfg.Broker("bearer-broker")
			if basic == nil || bearer == nil {
				t.Fatalf("expected both sentinel brokers to load, got basic=%v bearer=%v", basic, bearer)
			}
			assertContains("basic-broker.Auth.Username", basic.Auth.Username, sentBasicUser)
			assertContains("basic-broker.Auth.Password", basic.Auth.Password, sentBasicPass)
			assertContains("bearer-broker.Auth.Token", bearer.Auth.Token, sentBearerTok)

			out := captureStderr(t, func() {
				// Install a handler that writes to (the captured) os.Stderr so
				// slog output flows through the pipe. Save/restore the previous
				// default so later tests don't inherit a logger bound to the
				// closed pipe.
				prev := slog.Default()
				slog.SetDefault(slog.New(newSlogHandler(slog.LevelInfo)))
				t.Cleanup(func() { slog.SetDefault(prev) })

				logStartupBanners(cfg)
			})

			for _, s := range sentinels {
				if strings.Contains(out, s) {
					t.Errorf("startup banner leaked sentinel %q:\n%s", s, out)
				}
			}
		})
	}
}
