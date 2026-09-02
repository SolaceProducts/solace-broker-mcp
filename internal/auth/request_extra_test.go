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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const extraTestTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
const extraTestTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func observeRequestExtra(t *testing.T, extra *mcp.RequestExtra) (token string, tokenOK bool, corr string) {
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
	h := RequestExtraMiddleware()(next)
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
	token, ok, corr := observeRequestExtra(t, &mcp.RequestExtra{
		Header: extraHeader("Authorization", "Bearer subject-token-call", correlation.HeaderCorrelationID, "corr-tools-call"),
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
	_, _, corr := observeRequestExtra(t, &mcp.RequestExtra{
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
		token, ok, corr := observeRequestExtra(t, extra)
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
	token, ok, corr := observeRequestExtra(t, &mcp.RequestExtra{
		Header: extraHeader("Authorization", "Basic dXNlcjpwYXNz", correlation.HeaderCorrelationID, "corr-ok"),
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
	_, _, corr := observeRequestExtra(t, &mcp.RequestExtra{
		Header: extraHeader("Authorization", "Bearer tok"),
	})
	if corr != "" {
		t.Errorf("correlation ID = %q, want empty when Extra has no correlation header (do not generate here)", corr)
	}
}
