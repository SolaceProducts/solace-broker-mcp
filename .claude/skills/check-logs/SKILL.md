---
name: check-logs
description: Scan Go code for logging violations — credential leaks, missing LogValuer, raw structs, stdout usage, unsafe patterns
user_invocable: true
---

# Check Logs

Scan Go code for logging security violations and compliance issues. Invoked with `/check-logs`.

## When to use

- Before committing code
- After adding logging to a file
- As a periodic audit of the codebase
- When reviewing someone else's changes

## Usage

- `/check-logs` — scan the entire codebase
- `/check-logs <file_path>` — scan a specific file
- `/check-logs staged` — scan only git-staged files

## Steps

### Step 1: Read the rules

Read the secure logging rules at `docs/secure-logging-rules.md` in the project root. These are the rules you're checking against.

### Step 2: Identify files to scan

Based on the invocation:
- No args: scan all `.go` files under `cmd/` and `internal/`
- File path: scan that file
- `staged`: run `git diff --cached --name-only -- '*.go'` and scan those files

### Step 3: Run violation checks

For each file, check for the following violations in order of severity:

#### CRITICAL — Credential exposure

- [ ] **C-01: Credentials in log calls** — Any log call that includes `Password`, `Username`, `Token`, `Secret`, or `Authorization` field values
- [ ] **C-02: Raw struct logging** — `slog.Any("key", someStruct)` or `log.Printf("%+v", someStruct)` where the struct is or contains a credential-carrying type (`BrokerConfig`, `AuthConfig`, `ServerConfig`, `HTTPClient`)
- [ ] **C-03: Config map logging** — Logging `map[string]*BrokerConfig` or the full `ServerConfig.Brokers` map
- [ ] **C-04: Raw Authorization header** — Logging the value of an `Authorization` HTTP header
- [ ] **C-05: URL with credentials** — Logging a URL that matches `://.*:.*@` pattern

#### HIGH — Missing protections

- [ ] **H-01: Missing LogValuer** — Credential-carrying types (`AuthConfig`, `BrokerConfig`, and any struct with password/token/secret fields) that don't implement `slog.LogValuer`
- [ ] **H-02: Missing ReplaceAttr** — slog handler created without `ReplaceAttr` safety net
- [ ] **H-03: Stdout logging** — Any log output directed to `os.Stdout` instead of `os.Stderr`
- [ ] **H-04: Old log package** — Usage of `log.Printf`, `log.Println`, `log.Fatalf`, `log.Fatal` instead of `slog`

#### MEDIUM — Unsafe patterns

- [ ] **M-01: fmt.Sprintf in message** — `slog.Info(fmt.Sprintf(...))` or similar — variable data should be in structured fields, not the message
- [ ] **M-02: Raw external errors** — `slog.String("error", err.Error())` where the error comes from SEMP, HTTP, or external systems that might include credentials in error messages
- [ ] **M-03: String concatenation in message** — `slog.Info("action: " + userInput)` — use structured fields instead

#### LOW — Best practice

- [ ] **L-01: Missing required fields** — Tool invocation logs without all required fields (tool, broker, status, duration_ms)
- [ ] **L-02: Unstructured fmt.Print** — Any `fmt.Printf`, `fmt.Println` used for logging purposes

### Step 4: Report findings

Present findings grouped by severity with file path, line number, the offending code, and a fix suggestion.

### Output format

```
Log Security Check Results
==========================

Files scanned: 14
Violations found: 3

CRITICAL:
- internal/config/config.go:94 — H-04: Uses log.Printf (old log package)
  Code: log.Printf("WARNING: env_prefix naming convention...")
  Fix: Replace with slog.Warn("env_prefix naming convention is provisional")

HIGH:
- internal/config/config.go — H-01: AuthConfig does not implement slog.LogValuer
  Fix: Add LogValue() method that exposes only Method field

MEDIUM:
- (none)

LOW:
- cmd/server/main.go:35 — L-01: Startup log missing auth_mode field
  Code: log.Printf("Loaded config with %d broker(s)", len(cfg.Brokers))
  Fix: slog.Info("config loaded", slog.Int("broker_count", len(cfg.Brokers)), slog.String("auth_mode", "basic"))

Clean:
- internal/composite/ — no violations
- internal/registry/ — no violations
- internal/semp/sempv2/ — no violations
```

### Step 5: Summary

End with a one-line summary:
- All clear: "No violations found. Logging is compliant."
- Issues found: "Found N violations (X critical, Y high, Z medium). Fix critical and high before committing."

## Rules

- CRITICAL violations must be fixed before any commit — no exceptions
- HIGH violations should be fixed before commit
- MEDIUM and LOW are recommendations — flag them but don't block
- If you're unsure whether something is a violation, flag it as MEDIUM with a note explaining the uncertainty
- Never modify code during a check — only report. Use `/add-logs` to fix.
