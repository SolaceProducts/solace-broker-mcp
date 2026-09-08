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

package audit

import (
	"context"
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// authHook implements auth.AuthAuditHook (SOL-152097), translating
// TokenVerifier outcomes into auth_success/auth_failure records.
//
// It lives here rather than in internal/auth because this package already
// imports internal/auth for identity (PrincipalFrom, Principal); the
// interface it implements is what lets internal/auth trigger emission
// without importing this package back, which Go would refuse as a cycle.
// See auth.AuthAuditHook's doc for the full reasoning.
type authHook struct {
	enabled bool
}

// NewAuthHook returns the auth.AuthAuditHook cmd/server wires into
// auth.NewAuthMiddleware / auth.NewTokenVerifier. Pass
// Enabled(cfg.Observability) — the same flag and read site tools.WithAuditLog
// and resilience.WithAuditLog use for their own audit trails (SOL-152096) —
// so every audit gate in the process is read from one place. enabled=false
// makes every method a no-op, not a degraded emission.
func NewAuthHook(enabled bool) auth.AuthAuditHook {
	return &authHook{enabled: enabled}
}

// Success emits an auth_success record. info is projected into a Principal
// with auth.NewPrincipal — the same constructor auth.PrincipalMiddleware
// uses downstream for every tool call — and attached to a derived context
// so NewEvent's own ctx-only identity read (auth.PrincipalFrom) finds it:
// this hook fires from inside the TokenVerifier, before PrincipalMiddleware
// has ever touched ctx, so without this projection the record would carry
// no principal at all.
func (h *authHook) Success(ctx context.Context, info *sdkauth.TokenInfo) {
	if !h.enabled {
		return
	}
	emitAuthEvent(auth.WithPrincipal(ctx, auth.NewPrincipal(ctx, info)), Fields{Type: EventAuthSuccess})
}

// Failure emits an auth_failure record. sub and clientID are best-effort
// attribution from internal/auth (ClassifyAuthFailure's caller) — both ""
// unless the token parsed far enough to yield them, in which case a
// synthetic TokenInfo carries just those two claims through the same
// NewPrincipal projection Success uses. No principal is attached unless sub
// itself is non-empty: auth.Principal.Present() is true for any non-nil
// TokenInfo regardless of UserID, so gating on clientID alone would attach a
// principal carrying principal.sub="" — every other record in this schema
// omits an empty-valued field rather than emitting it (event.go), and a
// clientID with no sub is not enough attribution to be worth that
// exception. Both empty leaves the record with no principal at all, which
// is correct: the caller is unknown by definition here (EventAuthFailure's
// doc, event.go).
func (h *authHook) Failure(ctx context.Context, reason, sub, clientID string) {
	if !h.enabled {
		return
	}
	if sub != "" {
		info := &sdkauth.TokenInfo{UserID: sub, Extra: map[string]any{"client_id": clientID}}
		ctx = auth.WithPrincipal(ctx, auth.NewPrincipal(ctx, info))
	}
	emitAuthEvent(ctx, Fields{Type: EventAuthFailure, Reason: reason})
}

// emitAuthEvent builds and emits one auth_success/auth_failure record, or a
// drop notice when the constructor rejects fields — the same build-or-drop
// shape internal/tools/manager.go's emitOperationAudit uses, so every audit
// emission site in this codebase fails the same way.
func emitAuthEvent(ctx context.Context, fields Fields) {
	event, err := NewEvent(ctx, fields)
	if err != nil {
		slog.ErrorContext(ctx, "audit: auth record rejected by the schema constructor; recording a drop",
			slog.String("audit_event_type", string(fields.Type)),
			slog.String("detail", err.Error()))
		EmitDrop(ctx, DropContext{DroppedEventType: fields.Type})
		return
	}
	Emit(ctx, event)
}
