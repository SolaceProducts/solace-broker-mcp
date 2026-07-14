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

- `make dev-up-full` — idempotent, safe to run any time.  Brings up
  Keycloak + both Solace brokers + configures the OAuth profiles.  Use
  this for the full stack; `make dev-up` alone only starts Keycloak.
- `make oauth-down` / `make oauth-reset` — cleanup and rebuild targets
  for the Keycloak side.
- `./scripts/setup-oauth-brokers.sh` — idempotent broker setup.
  `dev-up-full` calls this; run it directly if only the brokers are
  what needs converging.
- `docker logs solace-broker-mcp-keycloak-oauth-dev --tail 50` — read
  Keycloak's log for real OAuth error causes instead of guessing.
- `curl -s -u admin:admin http://localhost:8081/SEMP/v2/config/oauthProfiles/keycloak_profile`
  (or port 8083 for solace-b) — read the broker's live OAuth profile.
  Same for `.../accessLevelGroups` to see what groups are mapped.
- `lsof -iTCP:19090 -sTCP:LISTEN -P` — check whether the MCP server is
  already running before assuming it isn't.
- `curl --cacert .local/certs/keycloak/keycloak.crt https://localhost:18443/...`
  — hit Keycloak directly with the right trust; never use `-k`.

## Correct sequence when the user says "connect my agent to the MCP server"

1. Confirm `make dev-up-full` succeeded (or run it if not).  This is
   what brings brokers up — `make dev-up` alone only starts Keycloak
   and does NOT configure the two Solace brokers.
2. Confirm `broker-config.oauth-test.yaml` exists at the repo root and
   has a real value for `mcp_server_client_auth.client_secret_basic.secret`
   (not the `REPLACE_WITH_MCP_SERVER_CLIENT_SECRET` placeholder).  If
   missing, walk the user through copying it from Keycloak.  See
   [README.md](./README.md) step 2 of first-time setup.
3. Confirm the MCP server is listening on `:19090` — check `lsof`.
   - If not, tell the user to `make run-oauth` in another terminal.
4. Confirm `solace-oauth-dev` is in `claude mcp list`.
   - If not, give the user the exact `claude mcp add` command.
5. Tell the user to relaunch Claude Code in a fresh terminal with
   `NODE_EXTRA_CA_CERTS` — do NOT try to relaunch it yourself.
6. Walk them through `/mcp` → login (`test-admin-user` / `password`).
7. Warn about the HSTS gotcha proactively if their browser is Safari.

## When diagnosing an OAuth failure

Three distinct failure classes.  Identify which BEFORE proposing fixes,
because each has a different diagnostic path.

**Class A — IdP-side (Keycloak rejected the token or the exchange).**
Symptoms: `token exchange failed`, `invalid_client`, `invalid_request`,
`Requested audience not available`, `subject_token validation failure`.

1. Read Keycloak's log FIRST (`docker logs ... --tail 50`).  The real
   error is almost always there in one line.
2. Grep for `TOKEN_EXCHANGE_ERROR`.  The `reason` field is authoritative.
3. Only THEN reason about causes.

**Class B — Broker-side, JWT accepted, permission denied.**
Symptoms: `HTTP 400 · semp_code 72`, `Command prohibited due to
Authorization Access Level`.  Distinguishing signal: the exchange log
line says success, then the tool call fails.

1. Which broker alias failed? Check the log's `broker` field.  The
   alias comes from `broker-config.oauth-test.yaml` — `prod-us` → the
   `solace` container, `test-us` → the `solace-b` container.
2. What groups does that broker have mapped?
   `curl -s -u admin:admin http://localhost:80{81,83}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups`.
3. What does the user's token carry in the `groups` claim?  Decode a
   fresh token (`kc.sh`-style curl + base64 decode of the middle
   segment) and inspect.  If missing, the `realm-roles-to-groups`
   mapper on `mcp-server-client` may have been removed or the user
   isn't in any relevant realm role.

**Class C — Callback / cert / transport (the tokens are fine, the
transport is broken).**
Symptoms: HSTS bumping the callback to HTTPS, PKCE code_challenge
corruption, `port 19090 already in use`, TLS verification failures.

1. Read the MCP server's log SECOND.  For a JWT rejection at Hop 1,
   the server logs the exact JOSE validation failure.
2. Failure modes seen so far — HSTS on the callback, invalid
   `code_challenge`, missing feature flag — each look identical from
   the outside but have very different fixes.  Match against README's
   Troubleshooting section rather than guessing.

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
