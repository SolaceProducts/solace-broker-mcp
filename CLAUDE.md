# Solace Broker MCP Server

## Tool naming

MCP tool names use **kebab-case** — e.g. `get-broker-status`, `list-queues`,
`get-redundancy-status`. This applies to every tool the server exposes,
regardless of whether it's defined in YAML (composite) or implemented in
Go (native SEMPv1/SEMPv2). Avoid `snake_case` and `camelCase`.

Match the in-code constant or `Name:` field exactly to the on-the-wire
tool name; LLMs see and pattern-match against this string.

## Before committing

Run `/check-logs` to scan for logging security violations. Fix all CRITICAL and HIGH issues before committing.
