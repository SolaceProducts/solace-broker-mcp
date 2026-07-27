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
//
// bindAddr is the effective host:port the server listens on. It is always
// logged so operators can confirm at a glance whether the (auth-mode-dependent)
// listener is loopback-only or network-reachable — the load-bearing fact for
// the disabled/static dev modes, which default to loopback.
func LogStartupAuthMode(mode, issuer, bindAddr string) {
	switch mode {
	case "disabled":
		slog.Warn(disabledBanner)
	case "static":
		slog.Warn(staticBanner)
	case "oauth":
		slog.Info("MCP client auth: OAuth/OIDC", slog.String("issuer", issuer))
	}
	slog.Info("MCP server bind address", slog.String("bind_address", bindAddr))
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

// LogStaticCleartextExposure warns that mode: static is serving its shared dev
// token over plaintext HTTP on a non-loopback interface — the caller decides
// when to emit it (see config.StaticTokenExposedCleartext). The token is
// long-lived and grants broker-admin-backed access, so on a routable, unencrypted
// listener a single packet capture is enough to replay it. This is a WARN, not a
// hard error: static is a dev mode and the network bind was deliberate. The fix
// is TLS (tls_cert_file/tls_key_file) or binding loopback. bindAddr is logged so
// operators can see exactly which interface is exposed.
func LogStaticCleartextExposure(bindAddr string) {
	slog.Warn(staticCleartextBanner, slog.String("bind_address", bindAddr))
}

const staticCleartextBanner = `
============================================================
  INSECURE MODE: static dev token on a plaintext network bind
  The shared dev token is sent unencrypted on a non-loopback
  interface and can be captured and replayed for broker-admin-
  backed access. Enable TLS (tls_cert_file/tls_key_file) or bind
  a loopback address.
============================================================`

// LogOAuthPlaintextListener warns that OAuth (production) mode is serving a
// plaintext listener under an explicit tls_terminated_upstream acknowledgment —
// the caller decides when to emit it (see config.OAuthPlaintextListenerAcknowledged).
// The listener carries client bearer tokens and tool results, so it must sit
// behind a TLS-terminating proxy/ingress; if none is in front, that traffic is
// on the wire in cleartext. This is a WARN, not a hard error: validate() already
// required the operator to acknowledge upstream termination, so the plaintext
// listener is a deliberate choice. The banner names the acknowledgment field so
// the WARN self-documents why the plaintext listener was allowed, and logs
// bindAddr so operators can confirm which interface is exposed.
//
// The banner does NOT escalate on a wildcard (all-interfaces) bind. Under oauth
// the listener defaults to all interfaces, and a wildcard bind is exactly the
// correct, common configuration behind a Kubernetes Service/ingress — the
// process cannot tell a locked-down pod netns from a LAN-exposed host, so an
// escalation there would fire loudest on the recommended deployment and train
// operators to ignore it. The bind-scope caveat is documented instead (see
// docs/authentication.md and the example configs).
func LogOAuthPlaintextListener(bindAddr string) {
	slog.Warn(oauthPlaintextListenerBanner, slog.String("bind_address", bindAddr))
}

const oauthPlaintextListenerBanner = `
============================================================
  OAuth mode on a plaintext listener (tls_terminated_upstream)
  The server is serving HTTP without TLS because
  tls_terminated_upstream: true acknowledges an upstream proxy
  or ingress terminates TLS. Client bearer tokens and tool
  results travel unencrypted on this listener — ensure the
  terminating proxy is actually in front of the bind address
  below, or set tls_cert_file/tls_key_file to terminate TLS
  at the server instead.
============================================================`

// LogHop2WithoutHop1 is the structural-mismatch headline for the Hop 1 /
// Hop 2 OAuth alignment invariant: if any broker uses auth.mode: oauth
// (Hop 2 — MCP server obtains a broker-bound token via RFC 8693 token
// exchange), then mcp_client_auth.mode must also be oauth (Hop 1 — the
// MCP server receives an agent token). The reason is mechanical: RFC 8693
// token exchange consumes the agent's Hop 1 token as the subject_token.
// Without Hop 1 OAuth there is no subject_token to exchange, and Hop 2
// has nothing to do.
//
// n is the count of brokers configured with auth.mode: oauth; hop1Mode is
// the current mcp_client_auth.mode value (one of "disabled" or "static" —
// "oauth" is the only valid Hop 1 mode for Hop 2 to work, and the empty
// case is already caught by the mcp_client_auth.mode-is-required
// validator before this banner can fire).
//
// This is a *headline* — it intentionally does not list broker names. The
// joined validation error (returned by config.validate() and logged by main)
// carries the per-broker context.
//
// This banner is PERMANENT: it enforces an invariant that holds for every
// release of the MCP server — Hop 2 OAuth structurally requires Hop 1 OAuth.
// config.validate() calls validateHop1Hop2Alignment unconditionally whenever
// any broker uses auth.mode: oauth, and this banner fires alongside the
// validation error whenever the invariant is violated.
func LogHop2WithoutHop1(n int, hop1Mode string) {
	noun := "1 broker"
	if n != 1 {
		noun = fmt.Sprintf("%d brokers", n)
	}
	slog.Error(fmt.Sprintf(hop2WithoutHop1Banner, noun, hop1Mode))
}

const hop2WithoutHop1Banner = `
============================================================
  OAuth broker authentication requires OAuth client
  authentication.

  Your config has %s with auth.mode: oauth, but
  mcp_client_auth.mode is %q. The MCP server needs the agent's
  token (received via mcp_client_auth) to obtain a broker
  token, so mcp_client_auth.mode must also be oauth. The
  server will not start.

  To proceed, either:
    - set mcp_client_auth.mode: oauth and configure issuer,
      audience, and resource_url, OR
    - change those brokers to use auth.mode: basic or bearer.
============================================================`
