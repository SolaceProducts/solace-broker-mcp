# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

Run from the **repo root** (`make entra-up`, `make entra-run`, …). Do not `cd` here.

## Before `make entra-up` / `make entra-run`

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. `make entra-up` and `make entra-run` preflight this.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate who already has the lab; it is not in git. Preflight fails on a missing or empty `.env` before brokers start.

3. Converge brokers, then run MCP (blocking) from a jwt-bearer tree:

   ```
   make entra-up
   make entra-run MCP_REPO=/Users/amitmorade/Desktop/projects/mcp+rag/solace-broker-mcp
   ```

   `entra-up` converges: certs (idempotent), brokers-up (re-PATCH), config. It does not start `go run`. Next steps print `make entra-run` and `make entra-claude-cmd`.

   `entra-run` is blocking. Do not start it from this checkout's default `MCP_REPO` (this tree is origin/main; jwt-bearer is not in `validGrantTypes`). Point `MCP_REPO=` at a tree that implements `GrantTypeJWTBearer` (typically `amorade/entra-prototype` in the sibling checkout above).

   `make entra-down` removes only these containers:

   | Container            | Host ports | Auth                         |
   | -------------------- | ---------- | ---------------------------- |
   | `mcp-entra-solace`   | 8081/1943  | Entra OAuth (prod-us)        |
   | `mcp-entra-solace-c` | 8082/1944  | basic `admin`/`admin`        |
   | `mcp-entra-solace-b` | 8083/1945  | Entra OAuth (test-us)        |

   Names are distinct from solace-local-infra `solace` / `solace-b`. **Do not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers — that script writes Keycloak issuer/JWKS and joins the Keycloak docker network. Entra brokers need outbound HTTPS to Microsoft for JWKS.

   If a docker container named `solace` (infra Keycloak lab) is running, preflight warns that ports 8081/1943 may collide.

## After the server is up

Claude is a separate process. `make entra-run` prints `NODE_EXTRA_CA_CERTS` and `NO_PROXY`. Export those, **restart Claude**, reconnect with mcp-agent Application (client) ID `REDACTED` (no client secret).

`make entra-claude-cmd` prints the full launch line.

## Coming back later

`make entra-up` again (converge: start what is stopped, re-PATCH the broker Entra profile).
`make entra-down` stops our containers; keeps `.local/` and `.env`.
`make entra-reset` when wedged: teardown containers then brokers-up (recreate). Does not delete `.env` or certs.
`make entra-certs-clean` only if TLS is wrong; then restart Claude.

## Generated files

Ignored under `local/entra/.local/` (certs, rendered YAML). `.env` is ignored. Do not commit either.
