# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

The rendered YAML is Hop 1 `mcp_client_auth` plus `broker_oauth` with `grant_type` token-exchange and per-broker `auth.audience`, with oauth aliases `prod-us` / `test-us` (and `local-basic` for admin/admin SEMP). This checkout loads that YAML, so `make entra` from this worktree is the command. `MCP_REPO` defaults to this repo; override only to point MCP at a different checkout (for example one that advertises `scopes_supported`). The template carries `jwt-bearer` and `auth.target` as comments for later.

**For an AI:** run every `make` from the **repo root**. Do not `cd local/entra`. Pick the target from the table below. Do not invent Keycloak commands. Do not run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers. Do not copy `MCPGODEBUG` into product, operator docs, or CI.

## Which `make` target

| Situation | Command | `MCP_REPO` |
| --- | --- | --- |
| First time this laptop, or come back and want brokers **and** MCP | `make entra` | Optional. Defaults to this checkout. |
| Brokers already up; restart MCP | `make entra` | Optional. Same as above. |
| Point MCP at a **different checkout** | `make entra MCP_REPO=/absolute/path/to/other-checkout` | Override. Other checkout must load the lab YAML (for example one that advertises `scopes_supported`). |
| Need certs + brokers + Entra SEMP PATCH, **not** the MCP process | `make entra-up` | No |
| Brokers stopped or profile drifted; keep the same containers | `make entra-up` again (converge, re-PATCH) | No |
| Brokers **exited** or wedged | `make entra-reset` then `make entra` | Same as entra |
| Done for the day; keep certs | `make entra-down` | No |
| MCP TLS cert is wrong | `make entra-certs-clean` then `make entra-up` (or `entra`); restart Claude | As for entra |
| Need Claude mcp remove/add + launch line | `make entra-claude-cmd` | No |
| Prove this laptop can run the brokers | `make entra-up` | No |

`MCP_REPO` is an **absolute path** to a solace-broker-mcp checkout whose `go run ./cmd/server` can load this lab YAML. The live YAML is `broker_oauth.grant_type` token-exchange with per-broker `auth.audience`, which this checkout loads — so `MCP_REPO` defaults to the repo you ran `make` from, and the override is only for pointing MCP at a different checkout (for example one that advertises `scopes_supported`). A typo'd override fails at preflight (`MCP_REPO is not a directory`).

`make entra-up` never starts MCP (`go run` blocks the terminal). `make entra` = `entra-up` then starts MCP.

`make entra-up` does **not** call Microsoft. It starts three brokers, installs the SEMP TLS cert, and PATCHes Entra issuer/JWKS/audience/group onto the oauth containers. The rendered YAML maps those containers to `prod-us`, `local-basic`, and `test-us`. SEMP HTTP on 28081/28082/28083 answering 200 with `admin`/`admin` means the laptop + Docker/Podman path works.

YAML decoding is strict. The live template is Hop 1 `mcp_client_auth` + `broker_oauth` token-exchange with per-broker `auth.audience`; the `jwt-bearer` and `auth.target` lines in the template are commented for later. A checkout picked via `MCP_REPO` must accept the same shape.

Claude sign-in is still Entra access-token validation. `get-broker-status` against `local-basic` is basic SEMP. `prod-us` / `test-us` are in this YAML (`auth.mode: oauth` + `broker_oauth` token-exchange for OBO), which this checkout serves.

The lab Host is `mcp-lab.solacetest.com` while the listener is loopback. `make entra` sets `MCPGODEBUG=disablelocalhostprotection=1` for that mismatch only. That env is not a product setting.

**`entra-reset` vs `entra-up`:** `entra-up` starts existing containers and re-PATCHes. `entra-reset` deletes and recreates the three `mcp-entra-solace*` names. It does not delete certs.

## Prerequisites (once per laptop)

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. `make entra` / `entra-up` preflight this.

2. **`.env`.** `cp local/entra/.env.example local/entra/.env` and fill every key (mcp-broker client secret plus tenant/app/group IDs). Ask a teammate; none of this is in git. Preflight fails on a missing `.env` or empty required keys before brokers start. The rendered YAML interpolates `${MCP_SERVER_CLIENT_SECRET}` into `broker_oauth`. Setup, YAML render, and `entra-claude-cmd` source `.env`. If the tenant or registrations change, edit `.env`, then `make entra-up` and restart MCP. Do not copy GUIDs into scripts, the YAML template, or git.

## First run

```
make entra
```

`MCP_REPO` defaults to this checkout. That: certs (idempotent), three brokers, Entra PATCH, render YAML, print the Claude line, then **block** on MCP.

`make entra` prints this block with an **absolute** cert path before it blocks on MCP. `make entra-claude-cmd` reprints it (certs must already exist). Copy those printed lines; do not invent a `…` path.

From **repo root**, after certs exist, the same commands are:

```
set -a && . local/entra/.env && set +a

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

No `--client-secret`. Quit Claude before the launch line (`NODE_EXTRA_CA_CERTS` is only read at process start). Sign in as `LAB_OPERATOR_UPN` from `.env` (`test-operator@solacetest.com`).

## Brokers

Bring-up copies infra’s `docker run` (image, shm, ulimits, 90s SEMP wait). Host ports are the Entra lab band so they do not steal infra `8081`/`1943`. Container names are `mcp-entra-solace*`. No Keycloak network. SEMP PATCH is Entra.

| Container            | Host ports  | This checkout's MCP alias |
| -------------------- | ----------- | ------------------------- |
| `mcp-entra-solace`   | 28081/21943 | prod-us (oauth / token-exchange OBO) |
| `mcp-entra-solace-c` | 28082/21944 | local-basic, `admin`/`admin` |
| `mcp-entra-solace-b` | 28083/21945 | test-us (oauth / token-exchange OBO probe) |

Do **not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these names — it writes Keycloak issuer/JWKS.

## Generated files

Ignored: `local/entra/.local/` (certs, rendered YAML), `local/entra/.env` (secret + tenant/app/group IDs). Do not commit them. Copy `local/entra/.env.example` (empty keys) to `.env`.
