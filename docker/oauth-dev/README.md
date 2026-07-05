# Local OAuth dev environment

Runs a Keycloak container (TLS, realm import) so the MCP server's OAuth path —
JWT validation on the way in, RFC 8693 token exchange on the way out — can be
exercised end to end against a real IdP without touching production.

All state (certs, container) lives under `.local/` at the project root, which
is gitignored. Nothing installs to `~/`. Nothing writes system-wide.

## Ports

Chosen to avoid the "everyone uses 8443" collision zone:

| Service            | Host port | In-container |
|--------------------|-----------|--------------|
| Keycloak HTTPS     | `18443`   | `8443`       |
| Keycloak HTTP      | `18180`   | `8080`       |
| MCP server (TLS)   | `19090`   | —            |

## One-time setup (fresh machine or fresh worktree)

Register the MCP server in Claude Code. If a prior registration exists (e.g.
from the manual hop1 setup), remove it first — the URL will have changed:

```bash
claude mcp remove solace-oauth-dev 2>/dev/null || true
claude mcp add --transport http --client-id agentic-app-client \
  solace-oauth-dev https://localhost:19090/mcp
```

## Every-time flow

```bash
# 1. Bring the stack up (certs + Keycloak + realm init).  Idempotent.
make dev-up

# 2. In a spare terminal — foreground server, Ctrl+C to stop.
make run-oauth

# 3. Launch Claude Code with the CA bundle.  Must be a fresh Claude Code
#    process — NODE_EXTRA_CA_CERTS is only read at startup.
NODE_EXTRA_CA_CERTS="$(pwd)/.local/certs/combined-ca-bundle.crt" claude
```

In Claude Code: `/mcp`, select `solace-oauth-dev`, log in with one of the
test users below.

## Test users

Both users are defined in `realm-export.json`.  Passwords are stripped
from realm exports on purpose (they'd leak into git otherwise); the
`keycloak-init.sh` script resets them every time the container starts.

| Username             | Password   | Role in the realm             |
|----------------------|------------|-------------------------------|
| `test-admin-user`    | `password` | admin — use this for the flow |
| `test-readonly-user` | `password` | reserved for RBAC scenarios   |

## Troubleshooting

### "Connection is not private" on `https://localhost:18443`
The Keycloak cert is self-signed.  Browsers don't trust it by default and
we deliberately do NOT install it into the system keychain.  Click through:
Safari → **Show Details** → **visit this website**; Firefox → **Advanced**
→ **Accept the Risk and Continue**.

### `https://localhost:PORT/callback` fails after login
HSTS bit you.  A previous session cached "always use HTTPS for localhost."
Claude Code's OAuth callback server runs plain HTTP on a random port, so
the browser upgrading to HTTPS breaks it.

Two fixes:
- **Immediate:** edit the URL bar, remove the `s` from `https`, hit Enter.
  The callback completes.
- **Permanent:** clear HSTS for `localhost`.  Safari → Settings → Privacy
  → Manage Website Data → search `localhost` → Remove.  Firefox →
  `about:config` → `network.stricttransportsecurity.preloadlist`.

Our `keycloak-init.sh` disables HSTS on every fresh container start, so
this only bites once you've already visited an HSTS-enabled Keycloak on
`localhost` before.

### `OAuth error: invalid_request - Invalid parameter: code_challenge`
Flaky.  Claude Code sometimes generates a PKCE `code_challenge` that
contains a character which gets corrupted somewhere in the transport
(a `+` collapsing to a space is the classic cause).  Just retry `/mcp` —
a fresh challenge usually works.

### MCP server refuses to start: "OAuth broker auth not supported"
`ENABLE_UNRELEASED_BROKER_OAUTH` isn't set.  `make run-oauth` sets it
automatically; if you're launching `go run ./cmd/server` by hand, export
it first.

### `podman-compose: error: unrecognized arguments: --wait`
You upgraded podman or switched compose runtimes.  The Makefile no longer
uses `--wait`; `oauth-init` health-gates on OIDC discovery instead. If
you see this, `git pull` — the Makefile fix is already in.

### `port 19090 already in use`
An earlier MCP server didn't shut down. Find it and kill it:
```bash
lsof -iTCP:19090 -sTCP:LISTEN -P
kill <PID>
```

## Verifying it's actually working

After `make dev-up`:

```bash
curl -sS --cacert .local/certs/keycloak/keycloak.crt \
  https://localhost:18443/realms/mcp-test-realm/.well-known/openid-configuration \
  | python3 -m json.tool | head -5
```

Should print the OIDC discovery document with `issuer` set to
`https://localhost:18443/realms/mcp-test-realm`.

After `make run-oauth`:

```bash
curl -sS --cacert .local/certs/mcp-server/mcp-server.crt \
  https://localhost:19090/.well-known/oauth-protected-resource \
  | python3 -m json.tool
```

Should return the protected-resource metadata pointing at the same Keycloak
issuer.

## Teardown

```bash
make oauth-down       # stops and removes the Keycloak container
make certs-clean      # deletes .local/certs/ — only needed if regenerating
```

Then in Claude Code, if you want to fully unlink:
```bash
claude mcp remove solace-oauth-dev
```
