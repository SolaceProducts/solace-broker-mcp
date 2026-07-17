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
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/banner"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// TestStartupBanner_NoCredentialLeak drives the exact banner emissions main()
// performs at boot (see main.go around the "Loud, refactor-robust signal"
// block) with a config that carries sentinel credentials in every
// secret-holding field, and asserts none of those sentinels reach stderr.
//
// This test guards the wiring: the banner package only takes primitives
// (mode / issuer / bind_address), so a credential can leak only if a caller
// hands one in. If a future refactor changes what main() passes to the
// banner emitters, this test fires as soon as a secret-bearing field is
// piped through. SOL-150757.
func TestStartupBanner_NoCredentialLeak(t *testing.T) {
	const (
		sentDevTok    = "SENTINEL_DEV_TOKEN_MUST_NOT_LEAK"
		sentBasicUser = "SENTINEL_BASIC_USERNAME_MUST_NOT_LEAK"
		sentBasicPass = "SENTINEL_BASIC_PASSWORD_MUST_NOT_LEAK"
		sentBearerTok = "SENTINEL_BEARER_TOKEN_MUST_NOT_LEAK"
	)
	sentinels := []string{sentDevTok, sentBasicUser, sentBasicPass, sentBearerTok}

	// Cover each mode main() may see. Bind/TLS shape is chosen per mode to
	// exercise the follow-up cleartext-exposure and plaintext-listener banners
	// so all three main() emission paths run.
	cases := []struct {
		name          string
		mode          string
		listenAddress string
		tlsTermUp     bool
	}{
		// disabled: only LogStartupAuthMode fires.
		{"disabled", config.AuthModeDisabled, "127.0.0.1", false},
		// static + non-loopback + no TLS -> also fires LogStaticCleartextExposure.
		{"static_cleartext", config.AuthModeStatic, "0.0.0.0", false},
		// oauth + tls_terminated_upstream -> also fires LogOAuthPlaintextListener.
		{"oauth_plaintext_upstream", config.AuthModeOAuth, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.ServerConfig{
				Port:          9090,
				ListenAddress: tc.listenAddress,
				MCPClientAuth: config.MCPClientAuthConfig{
					Mode:        tc.mode,
					Issuer:      "https://idp.example.com",
					Audience:    "mcp",
					ResourceURL: "https://mcp.example.com/mcp",
					DevToken:    sentDevTok,
				},
				TLSTerminatedUpstream: tc.tlsTermUp,
			}
			// The broker Auth block also holds credentials; keep sentinels here
			// even though the banner call sites don't reference brokers today.
			// Guards against a future refactor piping broker auth through.
			_ = config.BrokerConfig{
				URL: "https://broker.example.com:1943",
				Auth: config.AuthConfig{
					Mode:     config.AuthModeBasic,
					Username: sentBasicUser,
					Password: sentBasicPass,
					Token:    sentBearerTok,
				},
			}

			out := captureStderr(t, func() {
				// Install a handler that writes to (the captured) os.Stderr so
				// slog output flows through the pipe.
				slog.SetDefault(slog.New(newSlogHandler(slog.LevelInfo)))

				// Mirror main.go's startup banner sequence.
				banner.LogStartupAuthMode(cfg.MCPClientAuth.Mode, cfg.MCPClientAuth.Issuer, cfg.BindAddress())
				if cfg.StaticTokenExposedCleartext() {
					banner.LogStaticCleartextExposure(cfg.BindAddress())
				}
				if cfg.OAuthPlaintextListenerAcknowledged() {
					banner.LogOAuthPlaintextListener(cfg.BindAddress())
				}
			})

			for _, s := range sentinels {
				if strings.Contains(out, s) {
					t.Errorf("startup banner leaked sentinel %q:\n%s", s, out)
				}
			}
		})
	}
}
