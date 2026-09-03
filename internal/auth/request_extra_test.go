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
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const extraTestTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
const extraTestTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// validTokenInfo stands in for the *sdkauth.TokenInfo the SDK's
// RequireBearerToken attaches to Extra once it has validated the request.
// Tests that want the bearer-stamping path exercised (TokenInfo != nil) pass
// this; tests that want to exercise the "no verified token" path (auth mode
// "disabled", or a request that never went through bearer verification) pass
// nil, matching how PrincipalMiddleware itself branches on TokenInfo.
func validTokenInfo() *sdkauth.TokenInfo {
	return &sdkauth.TokenInfo{UserID: "auth0|test", Expiration: time.Now().Add(time.Hour)}
}

func observeRequestExtra(t *testing.T, correlationEnabled bool, extra *mcp.RequestExtra) (token string, tokenOK bool, corr string) {
	t.Helper()
	var (
		gotToken string
		gotOK    bool
		gotCorr  string
	)
	next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		gotToken, gotOK = RawSubjectTokenFromContext(ctx)
		gotCorr = correlation.From(ctx)
		return nil, nil
	}
	h := RequestExtraMiddleware(correlationEnabled)(next)
	req := &mcp.CallToolRequest{Extra: extra}
	if _, err := h(context.Background(), "tools/call", req); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return gotToken, gotOK, gotCorr
}

func extraHeader(pairs ...string) http.Header {
	h := make(http.Header)
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestRequestExtraMiddleware_StampsBearerAndCorrelation(t *testing.T) {
	t.Parallel()
	token, ok, corr := observeRequestExtra(t, true, &mcp.RequestExtra{
		Header:    extraHeader("Authorization", "Bearer subject-token-call", correlation.HeaderCorrelationID, "corr-tools-call"),
		TokenInfo: validTokenInfo(),
	})
	if !ok || token != "subject-token-call" {
		t.Errorf("subject token = %q ok=%v, want %q true", token, ok, "subject-token-call")
	}
	if corr != "corr-tools-call" {
		t.Errorf("correlation ID = %q, want %q", corr, "corr-tools-call")
	}
}

func TestRequestExtraMiddleware_TraceparentCorrelation(t *testing.T) {
	t.Parallel()
	_, _, corr := observeRequestExtra(t, true, &mcp.RequestExtra{
		Header: extraHeader(correlation.HeaderTraceparent, extraTestTraceparent),
	})
	if corr != extraTestTraceID {
		t.Errorf("correlation ID = %q, want trace-id %q", corr, extraTestTraceID)
	}
}

func TestRequestExtraMiddleware_NilExtraAndEmptyHeader_NoOp(t *testing.T) {
	t.Parallel()
	cases := []*mcp.RequestExtra{nil, {}, {Header: http.Header{}}}
	for _, extra := range cases {
		token, ok, corr := observeRequestExtra(t, true, extra)
		if ok || token != "" {
			t.Errorf("extra=%v: subject token = %q ok=%v, want empty", extra, token, ok)
		}
		if corr != "" {
			t.Errorf("extra=%v: correlation ID = %q, want empty", extra, corr)
		}
	}
}

func TestRequestExtraMiddleware_MalformedBearer_Skipped(t *testing.T) {
	t.Parallel()
	token, ok, corr := observeRequestExtra(t, true, &mcp.RequestExtra{
		Header:    extraHeader("Authorization", "Basic dXNlcjpwYXNz", correlation.HeaderCorrelationID, "corr-ok"),
		TokenInfo: validTokenInfo(),
	})
	if ok {
		t.Errorf("malformed bearer stored %q", token)
	}
	if corr != "corr-ok" {
		t.Errorf("correlation ID = %q, want still stamped", corr)
	}
}

func TestRequestExtraMiddleware_DoesNotGenerateCorrelation(t *testing.T) {
	t.Parallel()
	_, _, corr := observeRequestExtra(t, true, &mcp.RequestExtra{
		Header:    extraHeader("Authorization", "Bearer tok"),
		TokenInfo: validTokenInfo(),
	})
	if corr != "" {
		t.Errorf("correlation ID = %q, want empty when Extra has no correlation header (do not generate here)", corr)
	}
}

// TestRequestExtraMiddleware_NoTokenInfo_BearerSkipped pins that the bearer
// stamp requires extra.TokenInfo != nil — i.e. the SDK's RequireBearerToken
// already validated this request — mirroring PrincipalMiddleware's own gate.
// Without a verified TokenInfo (auth mode "disabled", or any request that
// never went through bearer verification), an Authorization header present on
// Extra must not be captured as the raw subject token, even though it is
// syntactically a well-formed bearer.
func TestRequestExtraMiddleware_NoTokenInfo_BearerSkipped(t *testing.T) {
	t.Parallel()
	token, ok, _ := observeRequestExtra(t, true, &mcp.RequestExtra{
		Header: extraHeader("Authorization", "Bearer unverified-token"),
		// TokenInfo intentionally nil.
	})
	if ok || token != "" {
		t.Errorf("subject token = %q ok=%v, want empty/false with no TokenInfo on Extra", token, ok)
	}
}

// TestRequestExtraMiddleware_CorrelationDisabled_NotStamped is the
// regression test for the blocking bug (Andrea, PR #371 review): with the
// OBS_CORRELATION_ID_ENABLED capability off, the HTTP correlation.Middleware
// is never wired onto the /mcp endpoint (see buildMCPEndpoint), so a client
// that supplies its own X-Correlation-ID or traceparent must not have it
// stamped onto the handler ctx here either. Before this fix, applyRequestExtra
// called correlation.With unconditionally whenever Extra carried a usable
// header, regardless of whether the capability was enabled — silently
// reviving correlation IDs (and everywhere they flow: logs,
// CallToolResult.Meta, outbound SEMP headers) even with the capability
// explicitly disabled.
func TestRequestExtraMiddleware_CorrelationDisabled_NotStamped(t *testing.T) {
	t.Parallel()

	t.Run("X-Correlation-ID", func(t *testing.T) {
		t.Parallel()
		_, _, corr := observeRequestExtra(t, false, &mcp.RequestExtra{
			Header: extraHeader(correlation.HeaderCorrelationID, "client-supplied-corr-id"),
		})
		if corr != "" {
			t.Errorf("correlation ID = %q, want empty when the capability is disabled, even though the client supplied %s", corr, correlation.HeaderCorrelationID)
		}
	})

	t.Run("traceparent", func(t *testing.T) {
		t.Parallel()
		_, _, corr := observeRequestExtra(t, false, &mcp.RequestExtra{
			Header: extraHeader(correlation.HeaderTraceparent, extraTestTraceparent),
		})
		if corr != "" {
			t.Errorf("correlation ID = %q, want empty when the capability is disabled, even though the client supplied a traceparent", corr)
		}
	})

	// Bearer propagation must stay unconditional on correlationEnabled — only
	// the correlation copy is gated. With a verified TokenInfo present, the
	// bearer stamp must still happen even though correlation is disabled.
	t.Run("bearer still stamped", func(t *testing.T) {
		t.Parallel()
		token, ok, corr := observeRequestExtra(t, false, &mcp.RequestExtra{
			Header:    extraHeader("Authorization", "Bearer subject-token-call", correlation.HeaderCorrelationID, "client-supplied-corr-id"),
			TokenInfo: validTokenInfo(),
		})
		if !ok || token != "subject-token-call" {
			t.Errorf("subject token = %q ok=%v, want %q true even with correlation disabled", token, ok, "subject-token-call")
		}
		if corr != "" {
			t.Errorf("correlation ID = %q, want empty when the capability is disabled", corr)
		}
	})
}
