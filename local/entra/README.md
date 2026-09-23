# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

Run from the **repo root**. Do not `cd` here.

One-time on the laptop: hosts line + `cp local/entra/.env.example local/entra/.env` with the mcp-broker secret. After that:

```
make entra
```

That converges certs, brokers (re-PATCH), config, then **blocks** on the MCP server. It picks `MCP_REPO` from this tree if jwt-bearer is implemented, otherwise `../solace-broker-mcp`. Override with `MCP_REPO=/path make entra`.

Claude is still a **second process** (`make` cannot restart it). `make entra` prints the Claude launch line **before** it blocks on MCP. Copy that line into a **fresh** terminal (quit Claude first). `make entra-claude-cmd` reprints it if you scrolled past.

Pieces if you need them: `make entra-up` (no `go run`), `make entra-run`, `make entra-down`.

## Before `make entra`

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. Preflight on `make entra` / `entra-up` / `entra-run`.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate who already has the lab; it is not in git. Preflight fails on a missing or empty `.env` before brokers start.

3. `make entra` as above. `entra-up` alone converges: certs (idempotent), brokers-up (re-PATCH), config, and does not start `go run`.

   `make entra-down` removes only these containers:

   | Container            | Host ports | Auth                         |
   | -------------------- | ---------- | ---------------------------- |
   | `mcp-entra-solace`   | 28081/21943 | Entra OAuth (prod-us)        |
   | `mcp-entra-solace-c` | 28082/21944 | basic `admin`/`admin`        |
   | `mcp-entra-solace-b` | 28083/21945 | Entra OAuth (test-us)        |

   Names are distinct from solace-local-infra `solace` / `solace-b`. **Do not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers — that script writes Keycloak issuer/JWKS and joins the Keycloak docker network. Entra brokers need outbound HTTPS to Microsoft for JWKS.

   Host ports are **not** 8081/1943 so this stack can sit beside solace-local-infra. If a docker container named `solace` is running, preflight still warns in case something else rebound the Entra ports.

## After the server is up

Use the Claude line printed before `go run`. Restart Claude with that env, then reconnect.

## Coming back later

`make entra` again (converge, then run). Or `make entra-up` if MCP is already running and you only need brokers re-PATCHed.
`make entra-down` stops our containers; keeps `.local/` and `.env`.
`make entra-reset` when wedged: teardown containers then brokers-up (recreate). Does not delete `.env` or certs.
`make entra-certs-clean` only if TLS is wrong; then restart Claude.

## Generated files

Ignored under `local/entra/.local/` (certs, rendered YAML). `.env` is ignored. Do not commit either.
