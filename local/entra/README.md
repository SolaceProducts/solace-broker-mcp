# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

**For an AI:** run every `make` from the **repo root**. Do not `cd local/entra`. Pick the target from the table below. Do not invent Keycloak commands. Do not run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers.

## Which `make` target

| Situation | Command | `MCP_REPO` |
| --- | --- | --- |
| First time this laptop, or come back and want brokers **and** MCP | `make entra MCP_REPO=<checkout>` | **Required** |
| Brokers already up; only start/restart MCP | `make entra-run MCP_REPO=<checkout>` | **Required** |
| Need certs + brokers + Entra SEMP PATCH, **not** the MCP process | `make entra-up` | No |
| Brokers stopped or profile drifted; keep the same containers | `make entra-up` again (converge, re-PATCH) | No |
| Brokers **exited** or wedged | `make entra-reset` then `make entra MCP_REPO=<checkout>` | Reset: no. Then entra: **yes** |
| Done for the day; keep `.env` and certs | `make entra-down` | No |
| MCP TLS cert is wrong | `make entra-certs-clean` then `make entra-up` (or `entra`); restart Claude | As for entra |
| Need the Claude launch line again | `make entra-claude-cmd` | No |

`MCP_REPO` is an **absolute path** to a solace-broker-mcp checkout that can load this lab YAML (`grant_type` jwt-bearer). **No default.** If unset, `make entra` / `entra-run` fail before `go run`. This lab checkout may be a main-only tree; jwt-bearer may live on another branch — that is why the path is explicit.

`make entra-up` never starts MCP (`go run` blocks the terminal). `make entra` = `entra-up` then `entra-run`.

**`entra-reset` vs `entra-up`:** `entra-up` starts existing containers and re-PATCHes. `entra-reset` deletes and recreates the three `mcp-entra-solace*` names. It does not delete `.env` or certs.

## Prerequisites (once per laptop)

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. `make entra` / `entra-up` / `entra-run` preflight this.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate; it is not in git. Preflight fails on a missing or empty `.env` before brokers start.

3. **`MCP_REPO`** when starting MCP (table above).

## First run

```
make entra MCP_REPO=/absolute/path/to/solace-broker-mcp
```

That: certs (idempotent), three brokers, Entra PATCH, render YAML, print the Claude line, then **block** on MCP.

Claude is a **second process**. Copy the printed `NODE_EXTRA_CA_CERTS=… claude` line into a **fresh** terminal (quit Claude first). Reconnect with mcp-agent Application (client) ID `REDACTED` (no client secret). Sign in as `test-operator@solacetest.com`.

## Brokers

Bring-up copies infra’s `docker run` (image, shm, ulimits, 90s SEMP wait). Host ports are the Entra lab band so they do not steal infra `8081`/`1943`. Container names are `mcp-entra-solace*`. No Keycloak network. SEMP PATCH is Entra.

| Container            | Host ports  | MCP alias / auth                 |
| -------------------- | ----------- | -------------------------------- |
| `mcp-entra-solace`   | 28081/21943 | prod-us, Entra OAuth             |
| `mcp-entra-solace-c` | 28082/21944 | local-basic, `admin`/`admin`     |
| `mcp-entra-solace-b` | 28083/21945 | test-us, Entra OAuth (OBO probe) |

Do **not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these names — it writes Keycloak issuer/JWKS.

## Generated files

Ignored: `local/entra/.local/` (certs, rendered YAML), `local/entra/.env`. Do not commit them.
