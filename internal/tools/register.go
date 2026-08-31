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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInternalErrorMessage is returned to the agent when a tool handler
// panics. Mirrors genericInternalMessage but names the server, not the
// broker, as the failing component; the panic detail stays server-side.
const serverInternalErrorMessage = "The server encountered an internal error executing this tool. Please try again later or contact your administrator."

// metaKeyCorrelationID is the CallToolResult.Meta key under which the request's
// correlation ID is surfaced back to the caller (SOL-151282), mirroring the
// X-Correlation-ID response header set by correlation.Middleware. The MCP
// protocol reserves the _meta object for exactly this kind of out-of-band
// metadata, so the value rides alongside the result without polluting the
// tool's structured output schema.
const metaKeyCorrelationID = "correlation_id"

// withRecovery wraps an SDK tool handler so a panic anywhere below the SDK
// boundary becomes a sanitized error result instead of killing the process.
// The SDK (go-sdk v1.5.0) invokes tool handlers on a goroutine of its own
// with no recover() — net/http's per-connection recovery cannot catch a
// panic on another goroutine — so without this wrapper a single panicking
// handler takes down the whole server (SOL-150685).
//
// This wrapper is the single chokepoint through which EVERY tool result flows
// back to the SDK — success, tool error, and panic-recovered alike — so it is
// also where the correlation ID is stamped onto CallToolResult.Meta. Stamping
// here (rather than across CallTool's many return paths) guarantees all three
// result kinds carry it. The HTTP request context that correlation.Middleware
// seeded reaches this handler's ctx (verified end-to-end through the
// StreamableHTTPHandler), so correlation.From(ctx) yields the request's ID;
// when the capability is off it returns "" and no Meta key is added.
func withRecovery(toolName string, h mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				// Log only the panic value's Go type, never its text: panic
				// values are unaudited and can carry arbitrary strings (the
				// same rule logToolResult applies to non-broker errors). The
				// stack trace pinpoints the panic site without echoing the
				// value, and the agent sees only the generic message below,
				// matching the unknown-error branch of buildErrorMessage.
				slog.Error("tool handler panicked",
					slog.String("tool", toolName),
					slog.String("panic_type", fmt.Sprintf("%T", r)),
					slog.String("stack", string(debug.Stack())))
				result = &mcp.CallToolResult{
					StructuredContent: map[string]any{
						"error":     serverInternalErrorMessage,
						"retryable": false,
					},
					Content: []mcp.Content{&mcp.TextContent{Text: serverInternalErrorMessage}},
					IsError: true,
				}
				err = nil
			}
			// Stamp the correlation ID onto the result on the way out. This
			// runs after both the normal return AND the panic-recovery branch
			// above, so success, error, and panic-recovered results are all
			// covered. A protocol-level error (err != nil, result nil) carries
			// no result body to annotate, so it is left untouched.
			stampCorrelationID(ctx, result)
		}()
		return h(ctx, req)
	}
}

// stampCorrelationID adds the request's correlation ID to result.Meta under
// metaKeyCorrelationID when both are present. It is a no-op when the capability
// is off (correlation.From returns "") or there is no result to annotate
// (protocol-level error). We own the correlation_id key and derive it fresh per
// request, so overwriting any prior value for that one key is intentional; Meta
// is initialized only when nil, so all other existing entries are preserved.
func stampCorrelationID(ctx context.Context, result *mcp.CallToolResult) {
	if result == nil {
		return
	}
	id := correlation.From(ctx)
	if id == "" {
		return
	}
	if result.Meta == nil {
		result.Meta = mcp.Meta{}
	}
	result.Meta[metaKeyCorrelationID] = id
}

// RegisterWithServer registers all tools from the ToolManager with the MCP
// server. For each handler it builds an mcp.Tool (with the broker parameter
// injected) and a closure that delegates to ToolManager.CallTool. This is
// the translation boundary between our ToolHandler / Metadata vocabulary and
// the SDK's mcp.Tool.
//
// When policy is non-nil, each handler is wrapped with withAuthorization
// inside withRecovery, so every dispatch consults the policy before the tool
// runs. groupsClaimName is the admin-configured JWT claim name the
// auth-middleware resolver was told to look for; the wrapper reports it on
// the missing-claim audit event so operators can jump straight to the IdP
// side of a day-one misconfiguration. When policy is nil, no authorization
// frame is composed and groupsClaimName is unused — the caller
// (cmd/server/main.go) owns the enable-gate and expresses disablement here
// as nil.
func RegisterWithServer(mgr *ToolManager, server *mcp.Server, pool *semp.BrokerPool, enableWriteTools bool, policy *authz.Policy, groupsClaimName string) {
	type registration struct {
		name    string
		handler ToolHandler
		meta    Metadata
	}

	handlers := mgr.Handlers()
	regs := make([]registration, 0, len(handlers))
	for _, h := range handlers {
		meta := h.Metadata()
		regs = append(regs, registration{name: meta.Name, handler: h, meta: meta})
	}
	// Sort by name for deterministic registration and tools/list ordering.
	sort.Slice(regs, func(i, j int) bool { return regs[i].name < regs[j].name })

	for _, reg := range regs {
		// Gate write/action tools behind the server's enable_write_tools
		// flag. Skipping at registration means the tool never appears in
		// tools/list and can't be invoked, regardless of client_auth mode.
		// Applies to any handler that is not read-only — native or composite —
		// so this is the single chokepoint for write-tool registration. Note
		// this gates ALL state-changing tools, including non-destructive ones
		// like clear-queue-stats: they still mutate broker state, so trial /
		// dev deployments expose only the read-only tool set by default.
		if isWriteTool(reg.meta.Annotations) && !enableWriteTools {
			slog.Info("write tool not registered (enable_write_tools is false)",
				slog.String("tool", reg.name))
			continue
		}

		mcpTool := toMCPTool(reg.meta, pool)

		var callToolHandler mcp.ToolHandler = func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			// Per-invocation audit identity from the SDK extras (SOL-149606).
			// Extra and TokenInfo can be nil in disabled mode or under test
			// scaffolding; NewIdentityFromTokenInfo handles nil cleanly.
			var info *sdkauth.TokenInfo
			if req.Extra != nil {
				info = req.Extra.TokenInfo
			}
			id := NewIdentityFromTokenInfo(info)

			// req.Params.Arguments carries omitempty on the wire, so a client
			// can send tools/call with no arguments field at all — treat that
			// the same as {} rather than failing json.Unmarshal on a nil/empty
			// RawMessage, mirroring describe_semp_schema.go's guard
			// (SOL-153765).
			var params map[string]any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
					// This closure bypasses ToolManager.CallTool, so without
					// this audit call a parse failure here would never reach
					// the audit surface at all (SOL-153765). Emit the same
					// "tool invoked" audit line CallTool's own defer would,
					// and return the manager's normal structured/retryable
					// error shape instead of a bare protocol error.
					//
					// toolErr (with the wrapped stdlib decode error) is
					// passed to logToolResult only — it logs unvouched
					// errors by Go type, never by text (secure-logging-
					// rules.md), so the raw decode message never reaches the
					// audit log. buildLocalErrorResult, in contrast, shows
					// err.Error() to the client verbatim, and its contract is
					// that every caller passes text this package wrote
					// itself — never text derived from client input — so it
					// gets a separate, static message rather than the
					// wrapped decode error.
					var brokerAlias string
					errorType := "bad_request"
					toolErr := fmt.Errorf("parsing tool arguments: %w", err)
					logToolResult(ctx, reg.name, &brokerAlias, start, &errorType, &toolErr, id)
					return buildLocalErrorResult(errors.New("tool arguments must be a JSON object")), nil
				}
			}
			return mgr.CallTool(ctx, reg.name, params, id)
		}

		// Compose withAuthorization INSIDE withRecovery so denials inherit
		// correlation-ID stamping and panic containment. Nil policy skips
		// the wrapper entirely — dispatch is byte-identical to pre-RBAC.
		if policy != nil {
			callToolHandler = withAuthorization(policy, reg.name, groupsClaimName, callToolHandler)
		}

		server.AddTool(mcpTool, withRecovery(reg.name, callToolHandler))
	}
}

// isWriteTool reports whether the annotation marks a tool that changes state
// (i.e. is not read-only). Every action tool — destructive (delete-queue-
// messages, disconnect-client) and non-destructive (clear-queue-stats,
// clear-client-stats) alike — mutates the broker, so all of them are gated
// behind enable_write_tools. Read-only monitoring tools always register.
func isWriteTool(a Annotations) bool {
	return !a.ReadOnly
}

// toMCPTool converts our Metadata to the SDK's mcp.Tool, injecting the broker
// parameter into the input schema. This is the only place in the codebase
// where our types and SDK types meet.
func toMCPTool(m Metadata, pool *semp.BrokerPool) *mcp.Tool {
	return &mcp.Tool{
		Name:         m.Name,
		Description:  m.Description,
		InputSchema:  injectBrokerParam(m.InputSchema, pool),
		OutputSchema: m.OutputSchema,
		Annotations:  toMCPAnnotations(m.Annotations),
	}
}

// toMCPAnnotations converts our Annotations to the SDK's mcp.ToolAnnotations.
func toMCPAnnotations(a Annotations) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    a.ReadOnly,
		DestructiveHint: a.Destructive,
		IdempotentHint:  a.Idempotent,
		OpenWorldHint:   a.OpenWorld,
	}
}

// RegisterListBrokers registers a list-brokers discovery tool that returns all
// configured broker aliases. This is a standalone tool, not a ToolHandler
// implementation — it does not call SEMP or require broker resolution.
func RegisterListBrokers(server *mcp.Server, pool *semp.BrokerPool) {
	server.AddTool(
		&mcp.Tool{
			Name:        "list-brokers",
			Description: "List all configured broker aliases. Use one of these as the 'broker' parameter on any other tool.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"brokers": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required": []string{"brokers"},
			},
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint: true,
			},
		},
		withRecovery("list-brokers", func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			// This handler does not flow through ToolManager.CallTool, so it
			// emits the "tool invoked" audit line itself — every tool
			// invocation must reach the audit surface (no broker attr: this
			// tool resolves none). Same panic-detection contract as
			// CallTool's defer: error returns set toolErr, the success
			// return sets result, both nil means a panic is unwinding.
			start := time.Now()
			var brokerAlias, errorType string
			var toolErr error
			var info *sdkauth.TokenInfo
			if req.Extra != nil {
				info = req.Extra.TokenInfo
			}
			id := NewIdentityFromTokenInfo(info)
			defer func() {
				if toolErr == nil && result == nil {
					errorType = "panic"
					toolErr = panicError{}
				}
				logToolResult(ctx, "list-brokers", &brokerAlias, start, &errorType, &toolErr, id)
			}()

			aliases := pool.Aliases()
			structured := map[string]any{"brokers": aliases}
			resultJSON, mErr := json.MarshalIndent(structured, "", "  ")
			if mErr != nil {
				errorType = "marshal_error"
				toolErr = fmt.Errorf("marshalling broker list: %w", mErr)
				return nil, toolErr
			}
			return &mcp.CallToolResult{
				StructuredContent: structured,
				Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
			}, nil
		}),
	)
}

// injectBrokerParam clones the given input schema and adds the required broker
// parameter with a description listing all available broker aliases.
//
// Currently performs a shallow clone of the schema map with deep copies of the
// mutated keys (properties, required). If future ToolHandler implementations
// use nested schema keywords (oneOf, if/then/else, $defs), this should be
// replaced with a full recursive deep copy.
func injectBrokerParam(schema map[string]any, pool *semp.BrokerPool) map[string]any {
	aliases := pool.Aliases()

	// Shallow-clone the schema to preserve all existing keywords.
	cloned := make(map[string]any, len(schema)+1)
	for k, v := range schema {
		cloned[k] = v
	}

	// Deep-copy properties (we're adding to this map).
	origProps, _ := schema["properties"].(map[string]any)
	properties := make(map[string]any, len(origProps)+1)
	for k, v := range origProps {
		properties[k] = v
	}
	properties["broker"] = map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("Target broker alias (required). Available brokers: %s", strings.Join(aliases, ", ")),
	}
	cloned["properties"] = properties

	// Deep-copy required list and prepend broker.
	required := []string{"broker"}
	if origRequired, ok := schema["required"].([]string); ok {
		required = append(required, origRequired...)
	}
	cloned["required"] = required

	return cloned
}
