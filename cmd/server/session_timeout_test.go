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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// testSessionIdleTimeout keeps the reaping tests fast. The SDK's behavior is
// independent of the value (production wires
// defaults.DefaultMCPSessionIdleTimeout in newMCPStreamableOptions).
const testSessionIdleTimeout = 100 * time.Millisecond

// initializeBody is a minimal MCP initialize request. The SDK issues an
// Mcp-Session-Id in response, which is what the reaping tests then track.
const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"2025-11-25","capabilities":{},` +
	`"clientInfo":{"name":"session-timeout-test","version":"0.0.1"}}}`

// newSessionTestHandler builds a genuine StreamableHTTPHandler with a short
// idle timeout, so tests observe the real SDK's session lifecycle rather than
// a mimic of it.
func newSessionTestHandler(timeout time.Duration) *mcp.StreamableHTTPHandler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "session-timeout-test",
		Version: "0.0.1",
	}, nil)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{SessionTimeout: timeout})
}

// newSessionRequest builds a POST /mcp request carrying the Accept and
// Content-Type headers the SDK validates, optionally bearing a session ID.
func newSessionRequest(body, sessionID string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	return req
}

// initializeSession drives one initialize exchange and returns the session ID
// the SDK minted for it.
func initializeSession(t *testing.T, handler *mcp.StreamableHTTPHandler) string {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSessionRequest(initializeBody, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d; body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize returned no Mcp-Session-Id; the stateful handler must issue one")
	}
	return sessionID
}

// sessionStatus replays a request under sessionID and reports the status the
// SDK answers with. 404 means the session is gone.
func sessionStatus(handler *mcp.StreamableHTTPHandler, sessionID string) int {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSessionRequest(initializeBody, sessionID))
	return rec.Code
}

// TestNewMCPStreamableOptions_SetsSessionIdleTimeout guards the whole point of
// SOL-153582: a zero SessionTimeout means the SDK never closes an idle
// session, so an abandoned session and its goroutines survive for the lifetime
// of the process. Passing nil options — the pre-fix state — reintroduces
// exactly that, which is why this asserts the field is populated rather than
// merely that the options are non-nil.
func TestNewMCPStreamableOptions_SetsSessionIdleTimeout(t *testing.T) {
	opts := newMCPStreamableOptions()

	if opts == nil {
		t.Fatal("newMCPStreamableOptions returned nil; idle sessions would never be reaped")
	}
	if opts.SessionTimeout <= 0 {
		t.Fatalf("SessionTimeout = %v, want > 0; zero means idle sessions are never closed",
			opts.SessionTimeout)
	}
	if opts.SessionTimeout != defaults.DefaultMCPSessionIdleTimeout {
		t.Errorf("SessionTimeout = %v, want %v",
			opts.SessionTimeout, defaults.DefaultMCPSessionIdleTimeout)
	}
}

// TestSessionTimeout_ReapsIdleSession pins the client-observable contract that
// SessionTimeout actually buys us: once the timeout elapses, the SDK closes
// the session and a request bearing its ID gets 404 "session not found". If an
// SDK upgrade changes when or whether idle sessions are reaped, this fails and
// the leak analysis can be re-verified against the new behavior.
func TestSessionTimeout_ReapsIdleSession(t *testing.T) {
	handler := newSessionTestHandler(testSessionIdleTimeout)
	sessionID := initializeSession(t, handler)

	// Wait without touching the session. Polling would defeat the test: every
	// request resets the idle timer (the SDK pauses it for the duration of an
	// in-flight request and restarts it on completion), so a poll loop keeps
	// the session alive forever and the assertion never fires.
	//
	// A fixed wait is safe here despite the usual flakiness objection: the
	// timer is armed for testSessionIdleTimeout regardless of machine load, so
	// a slow box makes reaping later in wall-clock terms but never earlier
	// than the deadline below. The margin is 20x.
	time.Sleep(20 * testSessionIdleTimeout)

	if status := sessionStatus(handler, sessionID); status != http.StatusNotFound {
		t.Fatalf("session %s still alive (status %d) well after the %v idle timeout; "+
			"idle sessions are not being reaped",
			sessionID, status, testSessionIdleTimeout)
	}
}

// TestSessionTimeout_KeepsActiveSession proves the timeout is an IDLE timeout,
// not a session lifetime cap. A session used more often than the timeout must
// survive indefinitely — otherwise the fix would cut off working clients mid
// conversation, which is the failure mode the 2-hour production value is
// chosen to avoid.
func TestSessionTimeout_KeepsActiveSession(t *testing.T) {
	handler := newSessionTestHandler(testSessionIdleTimeout)
	sessionID := initializeSession(t, handler)

	// Exercise the session across a span several times its idle timeout,
	// touching it more frequently than the timeout throughout.
	for range 10 {
		time.Sleep(testSessionIdleTimeout / 4)
		if status := sessionStatus(handler, sessionID); status != http.StatusOK {
			t.Fatalf("actively used session %s was closed (status %d); "+
				"SessionTimeout must bound idleness, not total lifetime",
				sessionID, status)
		}
	}
}
