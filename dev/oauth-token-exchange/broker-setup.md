# Broker setup for the local OAuth dev stack

The Keycloak dev stack ([README.md](./README.md)) authenticates the agent
to the MCP server. For the MCP server to then reach *brokers*, you need
two Solace broker containers configured as OAuth resource servers against
the same Keycloak realm.

This doc covers that broker side. Everything is automated by one script.

## Prerequisites

- `make dev-up` has succeeded — Keycloak is running on `https://localhost:18443`
  and the `solace-broker-mcp-oauth-dev_default` Docker network exists.
- Podman or Docker on PATH.
- No other containers using host ports **8081, 8083, 1943, or 1945**.

## What gets created

Two broker containers:

| Container   | SEMP HTTP (host) | SEMP TLS / SMF (host) | Required audience       | Alias in the MCP config |
|-------------|------------------|-----------------------|-------------------------|-------------------------|
| `solace`    | `8081`           | `1943`                | `solace-broker`         | `prod-us`               |
| `solace-b`  | `8083`           | `1945`                | `solace-broker-second`  | `test-us`               |

Both share the same OAuth profile shape:

- `oauthRole: resource-server`
- `issuer: https://localhost:18443/realms/mcp-test-realm`
- `endpointJwks: http://keycloak:8080/realms/mcp-test-realm/protocol/openid-connect/certs`
  (container-internal — the broker reaches Keycloak inside the Docker network)
- `resourceServerRequiredScope: solace.admin`
- `accessLevelGroupsClaimName: groups` (populated by `mcp-server-client`'s
  group mapper in `realm-export.json`)
- `usernameClaimName: sub`

The only per-broker difference is `resourceServerRequiredAudience`. That
mirrors the two audience mappers on `mcp-server-client`
(`solace-broker` and `solace-broker-second`), so the MCP server can prove
which broker a given exchanged token was minted for.

Two access-level groups on each broker's profile:

| Group name        | Global access | Purpose                                          |
|-------------------|---------------|--------------------------------------------------|
| `solace-admins`   | `admin`       | Full SEMP admin — the default for `test-admin-user` |
| `solace-readonly` | `read-only`   | Reserved for RBAC test scenarios                    |

Group membership flows from Keycloak realm roles → JWT `groups` claim via
`mcp-server-client`'s `realm-roles-to-groups` mapper.

## One command

From the repo root:

```bash
./scripts/setup-oauth-brokers.sh
```

The script is idempotent — safe to re-run at any time. It:

1. Creates (or starts) the two broker containers.
2. Attaches each to the Keycloak network so `http://keycloak:8080` resolves
   from inside them.
3. Polls each broker's SEMP endpoint until it's ready (up to 90 s per broker;
   a fresh Solace container takes ~30–60 s to accept SEMP).
4. Creates or patches the `keycloak_profile` on each broker with the exact
   fields listed above.
5. Adds the two access-level groups to each profile.
6. Reads back and verifies each broker's `resourceServerRequiredAudience`.

## Config for the MCP server

The MCP server needs a broker config that points at the two brokers plus the
OAuth wiring. The example is checked in; the real file is gitignored:

```bash
cp broker-config.oauth-test.example.yaml broker-config.oauth-test.yaml
```

Then edit the copy and replace the `REPLACE_WITH_MCP_SERVER_CLIENT_SECRET`
placeholder with the client secret for `mcp-server-client`.

The realm export ships with a **stable** client secret — every fresh
`make dev-up-full` run imports the same secret, so you set the config once
and it keeps working across resets. Grab the value the same way every time:

- Open [https://localhost:18443/admin/master/console/](https://localhost:18443/admin/master/console/)
- Login `admin` / `admin`
- Switch to the `mcp-test-realm` realm (top-left dropdown)
- Clients → `mcp-server-client` → **Credentials** tab → copy **Client Secret**

If you ever regenerate the secret through Keycloak's UI (or the realm
export changes), just repeat the copy step. There's no "make regen" that
touches this — `oauth-reset` re-imports the realm as-is.

`make run-oauth` reads the resulting file via
`CONFIG_FILE=broker-config.oauth-test.yaml`.

## Verification

After `./scripts/setup-oauth-brokers.sh`:

```bash
# Both brokers have the profile enabled
for port in 8081 8083; do
  echo "-- port $port --"
  curl -s -u admin:admin \
    "http://localhost:$port/SEMP/v2/config/oauthProfiles/keycloak_profile?select=enabled,resourceServerRequiredAudience" \
    | python3 -m json.tool
done
```

Expected: `enabled: true` on both, audiences `solace-broker` and
`solace-broker-second`.

After `make run-oauth` and Claude Code sign-in:

- `list-brokers` → returns `prod-us`, `test-us`
- `list-vpns` with `broker: prod-us` → succeeds
- `list-vpns` with `broker: test-us` → succeeds

## Troubleshooting

### `port 8081 already in use`
Another Solace container is already bound to that port. `docker ps` will
show which. Either stop it, or edit `scripts/setup-oauth-brokers.sh` to
use different host ports (and update `broker-config.oauth-test.yaml` to
match).

### `Keycloak network 'solace-broker-mcp-oauth-dev_default' not found`
You skipped `make dev-up`. Run it first — it creates the network as part
of bringing Keycloak up.

### `Command prohibited due to Authorization Access Level` on a tool call
The token reached the broker and was accepted, but the user's `groups`
claim didn't match any of the two mappings. Two things to check:

1. In Keycloak, is your user actually a member of a realm role named
   `solace-admins` or `solace-readonly`? (Users → your user → **Role
   mapping** tab.) The realm export puts `test-admin-user` in
   `solace-admins` by default.
2. Decode your token and confirm the `groups` claim is present. Read
   Keycloak's log for the last exchange; if the claim is missing, the
   mapper on `mcp-server-client` isn't firing for your user.

### `IdP error "invalid_request" - Requested audience not available`
The exchange succeeded in Keycloak's client-authentication step, but no
audience mapper on `mcp-server-client` matches the requested audience.
The realm export ships with mappers for both `solace-broker` and
`solace-broker-second`; if you added a third broker with a new audience,
add a matching mapper too:

- Clients → `mcp-server-client` → **Client scopes** → dedicated scope
  → **Mappers** → **Add mapper** → **Audience** → set **Included Custom
  Audience** to your new audience string. Save.

## Teardown

Stop and remove the broker containers:

```bash
docker stop solace solace-b
docker rm solace solace-b
```

The Keycloak side is unaffected — use `make oauth-down` from
[README.md](./README.md) for that.
