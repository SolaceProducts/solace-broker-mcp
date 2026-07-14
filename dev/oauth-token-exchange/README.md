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

## First-time setup

Follow these five steps in order on a fresh machine (or fresh worktree).
Everything is idempotent — safe to re-run if you get interrupted.

**1. Bring up Keycloak + brokers.**

```bash
make dev-up-full
```

This generates certs, starts Keycloak (imports the realm, resets user
passwords, disables HSTS), starts the two Solace brokers (`solace`,
`solace-b`), and configures their OAuth profiles + group mappings. If
`broker-config.oauth-test.yaml` doesn't exist yet, it's created from the
tracked template. Deep dive: [broker-setup.md](./broker-setup.md).

**2. Fill in the MCP-server client secret.**

Open `broker-config.oauth-test.yaml` (in the repo root) and replace the
`REPLACE_WITH_MCP_SERVER_CLIENT_SECRET` placeholder with the value from
Keycloak:

- Open [https://localhost:18443/admin/master/console/](https://localhost:18443/admin/master/console/)
  (click through the "not private" warning — this is the self-signed dev
  cert; **do not** install it into the system keychain).
- Login: `admin` / `admin`.
- Switch to `mcp-test-realm` (top-left dropdown).
- Clients → `mcp-server-client` → **Credentials** tab → copy **Client
  Secret** → paste into the config file.

The realm export ships with a stable secret, so this value survives
across `oauth-reset` cycles. You only need to re-copy if you rotate the
secret in Keycloak.

**3. Register the MCP server in Claude Code.**

```bash
claude mcp remove solace-oauth-dev 2>/dev/null || true
claude mcp add --transport http --client-id agentic-app-client \
  solace-oauth-dev https://localhost:19090/mcp
```

The `remove` is a safety net in case a previous registration (e.g. from
the older hop1 setup) points at a different URL.

**4. Start the MCP server in a spare terminal.**

```bash
make run-oauth
```

Foreground process — leave it running, Ctrl+C to stop. You'll come back
to this terminal to read server logs when debugging.

**5. Launch Claude Code with the CA bundle.**

In a fresh terminal (a *new* one — `NODE_EXTRA_CA_CERTS` is only read at
process start):

```bash
NODE_EXTRA_CA_CERTS="$(pwd)/.local/certs/combined-ca-bundle.crt" claude
```

Inside Claude Code:

- Run `/mcp`.
- Select `solace-oauth-dev`.
- A browser opens for Keycloak login. Sign in as
  `test-admin-user` / `password` (see the [Test users](#test-users) table).

Verify with a simple tool call in the chat: `list brokers` should return
`prod-us` and `test-us`. `list vpns for prod-us` should succeed.

## Every-time flow (after first-time setup)

Once step 2 (client secret) and step 3 (Claude Code registration) are done,
day-to-day work is just two commands:

```bash
# 1. Bring the stack back up.  Idempotent — no-op if everything's running,
#    starts what's stopped, reconfigures the OAuth profiles on the brokers.
make dev-up-full

# 2. In a spare terminal.
make run-oauth
```

Then in a fresh terminal:

```bash
NODE_EXTRA_CA_CERTS="$(pwd)/.local/certs/combined-ca-bundle.crt" claude
```

The MCP registration and the client secret survive across reboots and
`oauth-reset` cycles, so those two setup steps are truly one-time.

## Test users

Both users are defined in `realm-export.json` and pre-mapped to realm
roles (`solace-admins`, `solace-readonly`). Passwords aren't in the
export — `keycloak-init.sh` resets them to `password` every time the
container starts.

| Username             | Password   | Role assigned by realm import | Broker access |
|----------------------|------------|-------------------------------|---------------|
| `test-admin-user`    | `password` | `solace-admins`               | admin         |
| `test-readonly-user` | `password` | `solace-readonly`             | read-only     |

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
