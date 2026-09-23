# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

Run commands from the **repo root** (`make entra-up`). Do not `cd` here. Root aliases land in a later commit; until then this folder is the package only.

## Before `make entra-up`

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate who already has the lab; it is not in git.

3. Then `make entra-up` from the repo root (root aliases come in a later commit). Until then:

   ```
   make -C local/entra certs
   make -C local/entra brokers-up
   ```

   `brokers-down` removes only these containers:

   | Container            | Host ports | Auth                         |
   | -------------------- | ---------- | ---------------------------- |
   | `mcp-entra-solace`   | 8081/1943  | Entra OAuth (prod-us)        |
   | `mcp-entra-solace-c` | 8082/1944  | basic `admin`/`admin`        |
   | `mcp-entra-solace-b` | 8083/1945  | Entra OAuth (test-us)        |

   Names are distinct from solace-local-infra `solace` / `solace-b`. **Do not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers — that script writes Keycloak issuer/JWKS and joins the Keycloak docker network. Entra brokers need outbound HTTPS to Microsoft for JWKS.

## After the server is up

Claude is a separate process. Export `NODE_EXTRA_CA_CERTS` and `NO_PROXY` as printed by `entra-up`, **restart Claude**, reconnect with mcp-agent Application (client) ID `REDACTED` (no client secret).

## Coming back later

`make entra-up` again (converge: start what is stopped, re-PATCH the broker Entra profile). Until root aliases land, `make -C local/entra brokers-up` / `make -C local/entra brokers-down`.
`make entra-down` stops our containers; keeps `.local/` and `.env`.
`make entra-reset` when the broker is wedged.
`make entra-certs-clean` only if TLS is wrong; then restart Claude.

## Generated files

Ignored under `local/entra/.local/` (certs, rendered YAML). `.env` is ignored. Do not commit either.
