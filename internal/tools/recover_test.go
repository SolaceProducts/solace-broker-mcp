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
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWithPanicRecovery_PanicBecomesError(t *testing.T) {
	handler := withPanicRecovery("get-broker-health", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var m map[string]int
		m["boom"] = 1 // nil map write — representative latent handler bug
		return nil, nil
	})

	result, err := handler(context.Background(), &mcp.CallToolRequest{})

	if err == nil {
		t.Fatal("expected error from recovered panic, got nil")
	}
	if result != nil {
		t.Errorf("result = %v, want nil", result)
	}
	if !strings.Contains(err.Error(), "get-broker-health") {
		t.Errorf("error %q should name the tool", err.Error())
	}
	// The panic detail stays server-side (logs); the agent-facing error
	// must not echo internal panic values.
	if strings.Contains(err.Error(), "assignment to entry in nil map") {
		t.Errorf("error %q leaks internal panic detail", err.Error())
	}
}

func TestWithPanicRecovery_NormalCallPassesThrough(t *testing.T) {
	want := &mcp.CallToolResult{}
	wantErr := errors.New("broker unreachable")
	handler := withPanicRecovery("list-queues", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return want, wantErr
	})

	result, err := handler(context.Background(), &mcp.CallToolRequest{})

	if result != want {
		t.Errorf("result not passed through unchanged")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
