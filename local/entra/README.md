# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

Until root aliases land, run from the **repo root** with `make -C local/entra <target>`. Do not `cd` here.

## Before `make -C local/entra run`

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate who already has the lab; it is not in git.

3. Certs and brokers, then render + run:

   ```
   make -C local/entra certs
   make -C local/entra brokers-up
   make -C local/entra config
   make -C local/entra run MCP_REPO=/Users/amitmorade/Desktop/projects/mcp+rag/solace-broker-mcp
   ```

   `run` is blocking. Do not start it from this checkout's default `MCP_REPO` (this tree is origin/main; jwt-bearer is not in `validGrantTypes`). Point `MCP_REPO=` at a tree that implements `GrantTypeJWTBearer` (typically `amorade/entra-prototype` in the sibling checkout above).

   `brokers-down` removes only these containers:

   | Container            | Host ports | Auth                         |
   | -------------------- | ---------- | ---------------------------- |
   | `mcp-entra-solace`   | 8081/1943  | Entra OAuth (prod-us)        |
   | `mcp-entra-solace-c` | 8082/1944  | basic `admin`/`admin`        |
   | `mcp-entra-solace-b` | 8083/1945  | Entra OAuth (test-us)        |

   Names are distinct from solace-local-infra `solace` / `solace-b`. **Do not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers — that script writes Keycloak issuer/JWKS and joins the Keycloak docker network. Entra brokers need outbound HTTPS to Microsoft for JWKS.

## After the server is up

Claude is a separate process. `make -C local/entra run` prints `NODE_EXTRA_CA_CERTS` and `NO_PROXY`. Export those, **restart Claude**, reconnect with mcp-agent Application (client) ID `REDACTED` (no client secret).

`make -C local/entra claude-cmd` prints the full launch line.

## Coming back later

`make -C local/entra brokers-up` again (converge: start what is stopped, re-PATCH the broker Entra profile).
`make -C local/entra brokers-down` stops our containers; keeps `.local/` and `.env`.
`make -C local/entra certs-clean` only if TLS is wrong; then restart Claude.

Root aliases (`make entra-up` / `entra-down` / `entra-reset`) land in a later commit.

## Generated files

Ignored under `local/entra/.local/` (certs, rendered YAML). `.env` is ignored. Do not commit either.
