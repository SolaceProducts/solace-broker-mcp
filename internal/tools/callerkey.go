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

package tools

import (
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callerKeyFromRequest derives the identity a request's broker traffic is
// charged to for fair scheduling (SOL-153441).
//
// This is deliberately the ONLY place a caller key is built. Changing what
// fairness keys on — per tenant rather than per user, say — is then a change to
// this function and nothing else.
//
// # Why not reuse Identity
//
// Identity (identity.go) carries the same two OIDC concepts but is the
// audit-log projection: every field passes through sanitize.Claim, which caps
// values at 256 bytes, and an empty value collapses to the "<absent>"
// sentinel. Both are right for a log line and wrong for a map key. Two distinct
// subjects sharing a 256-byte prefix would truncate to the same string and
// merge into one fairness bucket, which is a way for one caller to take another
// caller's share. The raw TokenInfo.UserID has no such edge.
//
// # Why both levels
//
// Subject alone is not enough, and this is the case that matters in practice.
// Under an OAuth client_credentials grant the subject is a service account, so
// an agent platform fronting many user sessions behind one token collapses to a
// single bucket and fair scheduling does nothing in exactly the deployment
// shape that ships. Adding the MCP session ID subdivides that subject's share
// without opening a gaming hole: extra sessions split what the subject already
// has rather than multiplying it.
//
// # Empty values are not errors
//
// mcp_client_auth.mode: static gives every caller the subject "dev-user"
// (internal/auth/middleware.go), and mode: disabled supplies no TokenInfo at
// all, so Subject is shared or empty in both. Session is empty on any transport
// that carries no session ID. Each empty value simply collapses that level to
// one shared bucket, which is the documented behavior for the non-OAuth modes
// and needs no branch here or in the scheduler.
//
// Note the two independent axes this does NOT confuse: mcp_client_auth.mode
// governs caller-to-server identity and is what fairness keys on, while a
// broker's own auth.mode (basic/bearer/oauth) governs server-to-broker
// credentials and is irrelevant here. A broker on basic auth behind a server in
// mcp_client_auth.mode: oauth therefore gets full per-caller fairness.
func callerKeyFromRequest(req *mcp.CallToolRequest) resilience.CallerKey {
	var key resilience.CallerKey
	if req == nil {
		return key
	}
	// Extra and TokenInfo are both nil in disabled mode and under test
	// scaffolding that builds a bare CallToolRequest.
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		key.Subject = req.Extra.TokenInfo.UserID
	}
	// Session is nil only under test scaffolding; the SDK always populates it
	// on a real dispatch. ID() returns "" when the transport carries no session
	// ID, which this server's stateful streamable-HTTP handler does not do —
	// cmd/server/main.go leaves StreamableHTTPOptions.Stateless unset, so a
	// real MCP client always has one.
	if req.Session != nil {
		key.Session = req.Session.ID()
	}
	return key
}
