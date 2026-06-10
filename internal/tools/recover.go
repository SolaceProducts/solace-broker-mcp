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
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// withPanicRecovery wraps an MCP tool handler so a panic in any tool
// implementation is contained to that call instead of killing the process.
// The MCP SDK dispatches each request on a bare goroutine with no recover,
// so without this wrapper a single latent panic in one tool takes down every
// active session and all broker monitoring.
//
// The panic value and stack stay server-side in the log; the agent-facing
// error is generic so internal details (which may embed request state) never
// reach the client.
func withPanicRecovery(toolName string, next mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("tool handler panicked",
					slog.String("tool", toolName),
					slog.String("panic", fmt.Sprintf("%v", r)),
					slog.String("stack", string(debug.Stack())))
				result = nil
				err = fmt.Errorf("internal error in tool %q", toolName)
			}
		}()
		return next(ctx, req)
	}
}
