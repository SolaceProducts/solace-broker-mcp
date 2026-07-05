# Instructions for Claude — OAuth dev environment

You are helping the user with the local OAuth dev stack (Keycloak container +
MCP server + Claude Code client).  These are behavior rules for THIS
subsystem — user-facing setup docs are in `README.md` next to this file.

## Do not run these commands yourself

- **`make run-oauth`** — starts a long-running foreground server.  Ask the
  user to run it in a spare terminal, OR run it with your background-task
  tooling and remember to stop it when the user is done.  Never invoke it
  as a plain foreground `Bash` call; it will block until timeout.

- **`claude` (launching Claude Code)** — you ARE Claude Code.  Launching a
  new instance of yourself from a `Bash` tool call is nonsense.  When the
  user needs a fresh Claude Code process (e.g. to pick up
  `NODE_EXTRA_CA_CERTS`), tell them the exact command to run themselves.

- **`claude mcp add` / `claude mcp remove`** — these modify the user's
  Claude Code config on disk.  Don't run them; give the user the command
  to run themselves so they see and approve the config change.

## Do run these commands when useful

- `make dev-up` — idempotent, safe to run any time.  If the state is
  already correct it no-ops.
- `make oauth-down` / `make oauth-reset` — cleanup and rebuild targets.
- `docker logs solace-broker-mcp-keycloak-oauth-dev --tail 50` — read
  Keycloak's log for real OAuth error causes instead of guessing.
- `lsof -iTCP:19090 -sTCP:LISTEN -P` — check whether the MCP server is
  already running before assuming it isn't.
- `curl --cacert .local/certs/keycloak/keycloak.crt https://localhost:18443/...`
  — hit Keycloak directly with the right trust; never use `-k`.

## Correct sequence when the user says "connect my agent to the MCP server"

1. Confirm `make dev-up` succeeded (or run it if not).
2. Confirm the MCP server is listening on `:19090` — check `lsof`.
   - If not, tell the user to `make run-oauth` in another terminal.
3. Confirm `solace-oauth-dev` is in `claude mcp list`.
   - If not, give the user the exact `claude mcp add` command.
4. Tell the user to relaunch Claude Code in a fresh terminal with
   `NODE_EXTRA_CA_CERTS` — do NOT try to relaunch it yourself.
5. Walk them through `/mcp` → login (`test-admin-user` / `password`).
6. Warn about the HSTS gotcha proactively if their browser is Safari.

## When diagnosing an OAuth failure

1. Read Keycloak's log FIRST (`docker logs ... --tail 50`).  The real
   error is almost always there in one line.  Do not speculate before
   reading logs.
2. Read the MCP server's log SECOND.  For a JWT rejection, the server
   logs the exact JOSE validation failure.
3. Only THEN reason about causes.  The failure modes seen so far —
   HSTS on the callback, invalid `code_challenge`, missing feature
   flag — each look identical from the outside but have very different
   fixes.

## Never suggest

- **Installing certs into the macOS keychain.**  The user deliberately
  chose to keep this dev-only and avoid system-wide trust.  If browser
  warnings become annoying, point at the `trust-certs-macos` follow-up
  idea in README, but don't add it silently.
- **Committing anything under `.local/`.**  That directory is gitignored
  on purpose.  Certs, keys, rendered configs — all local artifacts.
- **`insecure_skip_verify` or `-k` in curl.**  Every party in this setup
  has a specific cert it should trust.  Sloppy TLS validation hides real
  bugs.
