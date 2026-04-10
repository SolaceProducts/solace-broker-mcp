---
name: add-logs
description: Add secure, compliant slog logging to Go code following the project's secure logging rules
user_invocable: true
---

# Add Logs

Add structured `log/slog` logging to Go code that follows the project's secure logging rules. Invoked with `/add-logs`.

## When to use

- After implementing a new feature or function that needs logging
- When migrating `log.Printf` calls to `slog`
- When adding logging to an existing file that lacks it

## Usage

- `/add-logs <file_path>` — add logging to a specific file
- `/add-logs <file_path> <function_name>` — add logging to a specific function

If invoked without arguments, ask the user which file or function to target.

## Steps

### Step 1: Read the rules

Read the secure logging rules at `docs/secure-logging-rules.md` in the project root. These rules are non-negotiable — every log line you write must comply.

### Step 2: Read the target code

Read the file or function the user specified. Understand what it does, what events are significant, and what data flows through it.

### Step 3: Identify loggable events

For each function, identify events that belong in the Always-Log list from the rules:

- Server startup/shutdown
- Tool invocations (tool name, broker alias, status, duration_ms)
- Tool errors (tool name, broker alias, error_type, http_status)
- Broker connections created (broker alias, URL)
- Config loaded (broker_count, port)
- Any error or failure path

### Step 4: Write the log calls

For each event, write a `slog` call following these constraints:

**Always:**
- Use `slog.Info()`, `slog.Warn()`, or `slog.Error()` — not `log.Printf`
- Use structured fields: `slog.String("key", value)`, `slog.Int("key", value)`, `slog.Duration("key", value)`
- Keep the message string static and descriptive (no variable data in the message)
- Include all required fields from the Always-Log table

**Never:**
- Never log anything on the Never-Log list (passwords, usernames, tokens, auth headers, raw config maps, URLs with embedded credentials)
- Never use `fmt.Sprintf` in the message string with external data
- Never log raw structs with `slog.Any("key", someStruct)` — log explicit fields
- Never log raw `err.Error()` from external systems (SEMP, HTTP) without reviewing what it could contain
- Never log to stdout — always stderr

### Step 5: Check for imports

Ensure the file imports `log/slog`. If it still imports `log`, check if any `log.Printf` calls remain. If not, remove the `log` import.

### Step 6: Present the changes

Show the user the changes with clear explanation of what each log line captures and why.

## Examples

### Good log calls

```go
// Tool invocation — all required fields present
slog.Info("tool invoked",
    slog.String("tool", toolName),
    slog.String("broker", brokerAlias),
    slog.String("status", "success"),
    slog.Duration("duration", elapsed))

// Error with structured context — no raw err.Error() from external source
slog.Error("SEMP call failed",
    slog.String("tool", toolName),
    slog.String("broker", brokerAlias),
    slog.String("operation", opID),
    slog.Int("http_status", statusCode))

// Startup — alias list, not config map
slog.Info("server starting",
    slog.String("port", cfg.Port),
    slog.Int("broker_count", len(cfg.Brokers)),
    slog.Any("broker_aliases", pool.Aliases()))
```

### Bad log calls (never write these)

```go
// BAD: raw struct may contain credentials
slog.Info("config loaded", slog.Any("config", cfg))

// BAD: fmt.Sprintf with external data in message
slog.Info(fmt.Sprintf("tool %s called", userInput))

// BAD: logging password
slog.Info("broker connected", slog.String("password", config.Auth.Password))

// BAD: raw error from SEMP may contain credentials
slog.Error("failed", slog.String("error", err.Error()))
```

## Rules

- Every log line must pass the Never-Log list check from `docs/secure-logging-rules.md`
- Every significant event must include the fields from the Always-Log table
- If unsure whether something is safe to log, don't log it — flag it for the user
- When migrating from `log.Printf`, don't just swap the call — redesign it with structured fields
