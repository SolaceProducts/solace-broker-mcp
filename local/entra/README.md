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
| Need Claude mcp remove/add + launch line | `make entra-claude-cmd` | No |

| Prove this laptop can run the brokers (no Entra prototype code) | `make entra-up` | No |

`MCP_REPO` is an **absolute path** to a solace-broker-mcp checkout that can load this lab YAML (`grant_type` jwt-bearer). **No default.** If unset, `make entra` / `entra-run` fail before `go run`. This lab checkout may be a main-only tree; jwt-bearer may live on another branch — that is why the path is explicit.

`make entra-up` never starts MCP (`go run` blocks the terminal). `make entra` = `entra-up` then `entra-run`.

**Without jwt-bearer (main / this lab tree as `MCP_REPO`):** `make entra-up` is the smoke test. It does **not** call Microsoft. It starts three brokers, installs the SEMP TLS cert, and PATCHes Entra issuer/JWKS/audience/group onto the oauth brokers. SEMP HTTP on 28081/28082/28083 answering 200 with `admin`/`admin` means the laptop + Docker/Podman path works.

Do **not** point `MCP_REPO` at main (or this tree if it still only allows RFC 8693). `go run` will refuse the lab YAML (`grant_type` jwt-bearer, `auth.target`). That is expected. Claude login, `get-broker-status`, and OBO are not in this smoke test — the MCP process is not talking to Entra yet.

**With a jwt-bearer-capable `MCP_REPO`:** Claude sign-in is Hop 1 (MCP validates an Entra access token). `local-basic` is Hop 1 + basic SEMP (still no OBO). `prod-us` is Hop 2 (MCP POSTs jwt-bearer to Entra, then SEMP with that token). `test-us` is the OBO deny probe.

**`entra-reset` vs `entra-up`:** `entra-up` starts existing containers and re-PATCHes. `entra-reset` deletes and recreates the three `mcp-entra-solace*` names. It does not delete `.env` or certs.

## Prerequisites (once per laptop)

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. `make entra` / `entra-up` / `entra-run` preflight this.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate; it is not in git. Preflight fails on a missing or empty `.env` before brokers start.

3. **Entra IDs.** Tenant, app, and group GUIDs are only in `local/entra/entra-ids.sh`. Setup, YAML render, and `entra-claude-cmd` read that file. If the tenant or registrations change, edit that file, then `make entra-up` and restart MCP. Do not copy GUIDs into scripts or the YAML template.

4. **`MCP_REPO`** when starting MCP (table above).

## First run

```
make entra MCP_REPO=/absolute/path/to/solace-broker-mcp
```

That: certs (idempotent), three brokers, Entra PATCH, render YAML, print the Claude line, then **block** on MCP.

`make entra` prints this block with an **absolute** cert path before it blocks on MCP. `make entra-claude-cmd` reprints it (certs must already exist). Copy those printed lines; do not invent a `…` path.

From **repo root**, after certs exist, the same commands are:

```
set -a && . local/entra/entra-ids.sh && set +a

claude mcp list
claude mcp remove solace-oauth-dev
claude mcp remove solace-entra-lab
# if it still appears: claude mcp remove <name> --scope user
#                         --scope project  or  --scope local

NODE_EXTRA_CA_CERTS="$PWD/local/entra/.local/certs/combined-ca-bundle.crt" \
NO_PROXY="$MCP_RESOURCE_HOST" \
  claude mcp add --transport http --client-id "$MCP_AGENT_CLIENT_ID" \
  solace-entra-lab "$MCP_RESOURCE_URL"

NODE_EXTRA_CA_CERTS="$PWD/local/entra/.local/certs/combined-ca-bundle.crt" \
NO_PROXY="$MCP_RESOURCE_HOST" claude
```

No `--client-secret`. Quit Claude before the launch line (`NODE_EXTRA_CA_CERTS` is only read at process start). Sign in as `LAB_OPERATOR_UPN` from `entra-ids.sh` (`test-operator@solacetest.com`).

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

Committed: `local/entra/entra-ids.sh` (tenant/app/group IDs, no secrets).
