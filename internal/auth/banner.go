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
	"log/slog"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// LogStartupBanner emits a prominent, refactor-robust signal of the configured
// client authentication mode. It lives in the boot path (called from
// cmd/server/main.go) — NOT in NewAuthMiddleware/NewTokenVerifier — for three
// reasons:
//
//  1. Single emission point — any future transport (stdio, gRPC) goes through
//     main and picks up the signal automatically.
//  2. Refactor-robust — surviving auth-path restructures means the signal
//     cannot vanish silently.
//  3. Visually loud — multi-line banner with INSECURE MODE callouts won't
//     get lost in request-log noise.
//
// For mode: disabled and mode: static the banner emits at WARN. For mode:
// oauth a single INFO log line is emitted; production is the unremarkable case.
//
// Do not move this into middleware. See SOL-149989 design spec at
// docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
func LogStartupBanner(cfg *config.ServerConfig) {
	switch cfg.MCPClientAuth.Mode {
	case config.AuthModeDisabled:
		slog.Warn(disabledBanner)
	case config.AuthModeStatic:
		slog.Warn(staticBanner)
	case config.AuthModeOAuth:
		slog.Info("MCP client auth: OAuth/OIDC", slog.String("issuer", cfg.MCPClientAuth.Issuer))
	}
}

const disabledBanner = `
============================================================
  INSECURE MODE: mcp_client_auth.mode = disabled
  Client → MCP server auth is OFF. Broker auth is unaffected.
  Development mode — NOT FOR PRODUCTION USE.
  Tool-invocation logs from this run are NOT a valid audit trail.
============================================================`

const staticBanner = `
============================================================
  INSECURE MODE: mcp_client_auth.mode = static
  Client → MCP server auth uses a static dev token. Broker auth is unaffected.
  Development mode — NOT FOR PRODUCTION USE.
  Tool-invocation logs from this run are NOT a valid audit trail.
============================================================`
