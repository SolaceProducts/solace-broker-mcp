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
	"context"
	"net/http"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RequestExtraMiddleware copies this JSON-RPC message's Extra.Header onto
// the handler ctx so hop 2 and correlation.From see this POST, not the
// initialize snapshot the stateful streamable session froze (SOL-153935).
//
// Extra is rebuilt per HTTP POST by the SDK. This middleware WithValue's
// onto this message's ctx only; it does not write on the session.
//
// Bearer is parsed from Authorization under rawSubjectTokenKey, the same
// key RawSubjectTokenFromContext already reads. Correlation is stamped via
// correlation.With when Extra carries a usable traceparent or
// X-Correlation-ID (HTTP correlation middleware publishes generated IDs
// onto the inbound header so Extra sees them). No ID is generated here:
// that would diverge from the HTTP-layer ID on the same POST.
func RequestExtraMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				ctx = applyRequestExtra(ctx, extra.Header)
			}
			return next(ctx, method, req)
		}
	}
}

func applyRequestExtra(ctx context.Context, h http.Header) context.Context {
	if token, ok := parseBearerToken(h.Get("Authorization")); ok {
		ctx = context.WithValue(ctx, rawSubjectTokenKey{}, token)
	}
	if id, ok := correlation.FromHeader(h); ok {
		ctx = correlation.With(ctx, id)
	}
	return ctx
}
