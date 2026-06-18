# SOL-150796 (T2) — Hop 2 OAuth Config Schema — Implementation Decisions

**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)
**Parent epic:** [SOL-150070](https://sol-jira.atlassian.net/browse/SOL-150070) — OAuth Token Exchange (Hop 2)

This file records decisions made *during implementation* of T2. It complements — and does not replace — the upstream architecture plan at [`docs/oauth/token-exchange-SOL-150070/architecture-plan.md`](../../../oauth/token-exchange-SOL-150070/architecture-plan.md).

**Format:** one section per decision, added as the corresponding code lands. Each section names the choice and the *reason* — what makes the entry useful three months later.

---

## T2 — Hop 1 client-auth block renamed: `client_auth` → `mcp_client_auth` (and Go type `ClientAuthConfig` → `MCPClientAuthConfig`)

**Date:** 2026-06-17
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

The earlier T2 design landed two blocks that both contain the word `client_auth`:

- The Hop 1 top-level block: `client_auth:` — *how agents authenticate to the MCP server*. The MCP server is the OAuth resource server here. Pre-existing in the codebase before T2.
- The Hop 2 nested sub-block: `broker_oauth.mcp_server_client_auth:` — *how the MCP server authenticates to the IdP*. The MCP server is the OAuth client here. Introduced by T2's discriminated-union refactor.

Both are correctly named for what they configure in isolation, but together they invite confusion. An operator skimming the YAML sees `client_auth` in two places and has to read carefully to understand which "client" is being authenticated.

### Decision

The Hop 1 block is renamed:

- YAML key: `client_auth` → `mcp_client_auth`.
- Go type: `ClientAuthConfig` → `MCPClientAuthConfig`.
- Go field on `ServerConfig` and `yamlConfig`: `ClientAuth` → `MCPClientAuth`.

The Hop 2 nested sub-block (`broker_oauth.mcp_server_client_auth`, of type `BrokerClientAuth`) is introduced under its renamed-aware name in a later commit. Its context — "inside `broker_oauth:`" — already disambiguates what kind of client is being authenticated.

### Why this is worth doing

1. **The Hop 1 block is the one whose name needs the qualifier.** In context, "the MCP server's client auth config" answers the question *whose client* is being authenticated — it's the MCP client (the agent) authenticating to the MCP server. `mcp_client_auth` makes that explicit. Without the qualifier, an operator coming to the file fresh sees `client_auth` and has to figure out which "client" it refers to.

2. **The Hop 2 sub-block's context disambiguates it.** `broker_oauth.mcp_server_client_auth` is read as "broker-OAuth client-auth" — the MCP server's client identity at the IdP, used for broker OAuth. The `broker_oauth:` parent already establishes "this is about authenticating to the IdP for broker access," so `mcp_server_client_auth` inside that context unambiguously means "how the MCP server identifies itself *as a client* of the IdP." No rename needed.

3. **The asymmetry is a feature, not a bug.** The Hop 1 block is at the top level of the YAML — there's no parent block to provide context. It needs the qualifier embedded in its own name. The Hop 2 sub-block lives inside `broker_oauth:`, which provides the context. Symmetric renaming (e.g., renaming both to `mcp_*`) would over-qualify the nested one.

### What this commits us to

- `mcp_client_auth:` is the V1 contract for the Hop 1 block. It will not be renamed again without a deprecation cycle.
- The Go type `MCPClientAuthConfig` and field `MCPClientAuth` are internal API surface but the naming is now stable.
- Existing internal operator deployments using `client_auth:` must update their YAML. We are pre-V1, so this is a tolerable break — but it is the kind of break that informed our other "design once, commit forever" decisions in T2.

### Pre-V1 license — used deliberately

This rename uses some of our pre-V1 schema-change budget. We did so because:

- The disambiguation pays off forever in operator UX.
- Doing it now (before V1) costs only internal redeployment of operator YAML; doing it post-V1 would cost public deprecation and migration tooling.
- The cost ratio is small now, large later.

That is exactly the cost-curve calculation the V1 contract commitment exists to capture. We spent the budget where it bought the most.
