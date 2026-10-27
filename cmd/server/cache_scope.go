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

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cacheScopePrivate is the 2026-07-28 spec's cacheScope for a result that may
// vary per user: only the requesting user's client may store it. The SDK has no
// exported constant for either value.
const cacheScopePrivate = "private"

// newServerOptions builds the SDK options for the MCP server.
//
// Only the cache-control hint is set. Everything else stays at the SDK default,
// which is what main() got when it passed nil.
func newServerOptions(cfg *config.ServerConfig) *mcp.ServerOptions {
	return &mcp.ServerOptions{
		SetCacheable: toolsListCacheable(config.ToolAuthorizationEnabled(cfg)),
	}
}

// toolsListCacheable returns the SetCacheable hook that labels tools/list
// results "private" when toolAuthorization is on, and leaves them at the SDK's
// "public" default when it is off.
//
// The spec defines "public" as free of user-specific data and says a public
// result from an authenticated endpoint may be shared across access tokens. With
// tool authorization on, the list is a per-caller view (WithListFiltering), so
// "public" would mislabel it for any gateway that ignores ttlMs.
//
// Keyed on authorization being enabled, not on filter_tools_list: private is
// the safe label, and the filter flag can then be flipped without the label
// changing under a cache.
//
// The decision is a function of the server's configuration alone, never of the
// request or the cursor, so every page of one list carries the same scope.
//
// ttlMs is deliberately left at the SDK default of 0 ("immediately stale"): the
// list changes with the caller's groups and with a policy edit plus restart, and
// the server has no way to tell a client that either happened. A positive TTL
// would be a staleness bug waiting on the first policy change.
//
// SetCacheable also fires for server/discover and every other list method.
// Those are left alone: discover is the same for every caller, and the server
// registers no prompts or resources. The type assertion is what keeps this change
// to tools/list.
//
// The SDK calls it with the server's mutex held for the four list methods, so it
// must not call back into the server.
func toolsListCacheable(toolAuthorization bool) func(context.Context, mcp.Request, *mcp.Cacheable) {
	return func(_ context.Context, req mcp.Request, c *mcp.Cacheable) {
		if !toolAuthorization {
			return
		}
		if _, ok := req.(*mcp.ListToolsRequest); ok {
			c.CacheScope = cacheScopePrivate
		}
	}
}
