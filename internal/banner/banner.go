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

// Package banner is the home for the MCP server's operator-facing startup and
// validation banners — the prominent multi-line log lines operators see in
// boot output and config-validation failures.
//
// Banners live here, separate from the packages whose logic triggers them,
// for three reasons:
//
//  1. ONE CANONICAL HOME. Anyone searching for "where are operator banners
//     defined?" finds the answer in a single file. Before this consolidation,
//     banners lived in internal/auth/banner.go (startup auth-mode signal)
//     AND inside internal/config/config.go (config-validation banners).
//     Spreading banners across whichever package happened to trigger them
//     made discovery harder and invited further drift as new banners landed.
//
//  2. NO IMPORT CYCLES. internal/config defines the types operators write in
//     YAML; internal/auth uses those types. If banners lived in either of
//     those packages, the other one couldn't call into them without
//     introducing a cycle. A neutral third package solves the topology
//     cleanly: both config and auth can import banner; banner depends on
//     neither.
//
//  3. PACKAGE BOUNDARY ENFORCES SEPARATION. Banners are operator-facing UX
//     strings — they don't need anything from config or auth except the data
//     the caller passes in. Living in a dedicated package makes that contract
//     structural rather than a convention.
//
// All emitters take primitive parameters (strings, ints) rather than richer
// types from other packages. That keeps the package free of internal
// dependencies and means callers explicitly choose what to expose. Mirrors
// how internal/defaults works for cross-cutting default values.
package banner

import (
	"fmt"
	"log/slog"
)

// LogStartupAuthMode emits the operator-facing signal of the configured Hop 1
// (MCP client → MCP server) authentication mode at server boot. It runs from
// cmd/server/main.go — NOT from middleware — for three reasons:
//
//  1. Single emission point — any future transport (stdio, gRPC) goes through
//     main and picks up the signal automatically.
//  2. Refactor-robust — surviving auth-path restructures means the signal
//     cannot vanish silently.
//  3. Visually loud — multi-line banner with INSECURE MODE callouts won't
//     get lost in request-log noise.
//
// For mode: "disabled" and mode: "static" the banner emits at WARN. For
// mode: "oauth" a single INFO log line is emitted; production is the
// unremarkable case.
//
// The mode and issuer arguments come directly from the validated
// mcp_client_auth block. The caller unpacks them rather than passing a
// ServerConfig pointer so this package stays free of internal/config
// dependencies. See SOL-149989 design spec at
// docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
func LogStartupAuthMode(mode, issuer string) {
	switch mode {
	case "disabled":
		slog.Warn(disabledBanner)
	case "static":
		slog.Warn(staticBanner)
	case "oauth":
		slog.Info("MCP client auth: OAuth/OIDC", slog.String("issuer", issuer))
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

// LogOAuthNotSupported is the V1 not-yet-supported guard headline. It is
// logged when any broker is configured with auth.mode: oauth, which the
// schema accepts but the V1 runtime cannot use. The n argument is the count
// of affected brokers; the function formats it with the correct
// singular/plural form ("1 broker" vs "N brokers").
//
// The banner is logged via slog.Error as a SEPARATE log line from the joined
// validation error — operators see the headline first, then the comprehensive
// error with broker names. Wording is operator-language: what we detected,
// why it failed, what to do today, and that the feature is planned.
//
// This is a *headline* — it intentionally does not list broker names. The
// joined validation error (returned by config.validate() and logged by main)
// carries the per-broker rejection messages with the broker aliases, so
// operators have the names there. Keeping the headline broker-name-free
// means the banner scales: 1 affected broker or 47, the headline stays the
// same shape.
//
// LIFECYCLE — REMOVE WHEN THE OAUTH RUNTIME LANDS:
// This banner, the validateBroker check inside internal/config that produces
// oauthBrokerCount, and the call site in config.validate() are all part of
// the same temporary guard. They exist only because V1 ships the
// broker_oauth schema without the runtime that consumes it. When the
// runtime sub-ticket (SOL-150070 follow-ups: token exchanger + oauth
// Authenticator + cookie jar) lands and the per-broker oauth flow actually
// works, delete all three together.
func LogOAuthNotSupported(n int) {
	noun := "1 broker"
	if n != 1 {
		noun = fmt.Sprintf("%d brokers", n)
	}
	slog.Error(fmt.Sprintf(oauthNotSupportedBanner, noun))
}

const oauthNotSupportedBanner = `
============================================================
  This version of the MCP server does not yet support
  authenticating to brokers using OAuth.

  Your config has %s with auth.mode: oauth, which the server
  recognizes but cannot use. The server will not start.

  To proceed today, change those brokers to use auth.mode:
  basic (username + password) or auth.mode: bearer (static
  token). Both are fully supported in this version.

  OAuth broker authentication is planned in a future release.
============================================================`
