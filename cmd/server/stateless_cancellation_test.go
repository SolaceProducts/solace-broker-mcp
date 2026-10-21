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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cancellationWait bounds every wait in this file. The tool blocks only on its
// own context, so a handler that is never cancelled shows up as a timeout
// rather than a hang.
const cancellationWait = 5 * time.Second

// TestStatelessTransport_ClientDisconnectCancelsToolContext proves that when a
// 2026-07-28 client drops a request mid-call, the tool handler's context is
// cancelled. Without PropagateRequestCancellation the handler keeps running for
// a client that has gone.
//
// The handler comes from newMCPStreamableOptions(true), so removing the option
// there turns this red. The test server is not the production tool set: it
// proves the SDK option and our wiring of it, not any production tool.
func TestStatelessTransport_ClientDisconnectCancelsToolContext(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan error, 1)
	// release lets cleanup free a tool whose context was never cancelled.
	// Without it, a regression would leave the handler blocked and ts.Close
	// would hang the whole package instead of failing this test.
	release := make(chan struct{})

	server := mcp.NewServer(&mcp.Implementation{Name: "cancellation-test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "block", Description: "blocks until its context is cancelled"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			close(started)
			select {
			case <-ctx.Done():
				ended <- ctx.Err()
				return nil, struct{}{}, ctx.Err()
			case <-release:
				return nil, struct{}{}, errors.New("released by test cleanup")
			}
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		newMCPStreamableOptions(true))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), cancellationWait)
	defer cancelConnect()
	client := mcp.NewClient(&mcp.Implementation{Name: "cancellation-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(connectCtx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// The option only applies on the 2026-07-28 protocol. If the client fell
	// back to an older version, a missing option and a present one look the same.
	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol %q, want 2026-07-28; the cancellation path would not be exercised", got)
	}

	// Registered last, so it runs first: free the tool before the session and
	// the server wait on it.
	t.Cleanup(func() { close(release) })

	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	go func() {
		_, _ = session.CallTool(callCtx, &mcp.CallToolParams{Name: "block"})
	}()

	select {
	case <-started:
	case <-time.After(cancellationWait):
		t.Fatal("tool handler never started; the request did not reach it, so this proves nothing about cancellation")
	}

	cancelCall() // the client drops the request mid-call

	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("tool context ended with %v, want context.Canceled", err)
		}
	case <-time.After(cancellationWait):
		t.Fatal("tool handler context was not cancelled after the client disconnected")
	}
}
