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

package metrics

import (
	"context"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// AuthHook is auth.AuthAuditHook's method set, restated here so this package
// does not import internal/auth (and can never be part of a cycle with the
// packages auth depends on). Go's structural interfaces make the two
// interchangeable at the cmd/server wiring site.
type AuthHook interface {
	Success(ctx context.Context, info *sdkauth.TokenInfo)
	Failure(ctx context.Context, reason, sub, clientID string)
}

// CountingAuthHook decorates next so every Failure also increments
// mcp_auth_failure_total with the reason it receives — the same reason the
// auth_failure audit record carries, classified once inside the TokenVerifier
// (see auth.AuthAuditHook). Success delegates without counting. A nil sm
// returns next unchanged; next must be non-nil (cmd/server always passes
// audit.NewAuthHook, which never is).
func CountingAuthHook(sm *SecurityMetrics, next AuthHook) AuthHook {
	if sm == nil {
		return next
	}
	return &countingAuthHook{sm: sm, next: next}
}

type countingAuthHook struct {
	sm   *SecurityMetrics
	next AuthHook
}

func (h *countingAuthHook) Success(ctx context.Context, info *sdkauth.TokenInfo) {
	h.next.Success(ctx, info)
}

// Failure widens the interface's plain-string reason to the vocabulary type
// here, the same untrusted-boundary step audit.authHook.Failure performs;
// every producer is auth.ClassifyAuthFailure, which only returns vocabulary
// values, so this is a restatement of type rather than a validation.
func (h *countingAuthHook) Failure(ctx context.Context, reason, sub, clientID string) {
	h.sm.RecordAuthFailure(ctx, schema.AuthFailureReason(reason))
	h.next.Failure(ctx, reason, sub, clientID)
}
