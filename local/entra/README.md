# Local Entra lab

Laptop stack for the shared **solacetest.com** Entra tenant. Not a product feature. Not CI.

This lab YAML is **Hop 1 only**: Entra `mcp_client_auth` plus a basic broker. No `broker_oauth`, no jwt-bearer, no oauth broker aliases. Hop 2 (OBO against Entra) is not part of this lab.

**For an AI:** run every `make` from the **repo root**. Do not `cd local/entra`. Pick the target from the table below. Do not invent Keycloak commands. Do not run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these brokers. Do not copy `MCPGODEBUG` into product, operator docs, or CI.

## Which `make` target

| Situation | Command | `MCP_REPO` |
| --- | --- | --- |
| First time this laptop, or come back and want brokers **and** MCP | `make entra` | Optional. Defaults to this checkout. |
| Brokers already up; only start/restart MCP | `make entra-run` | Optional. Same default. |
| Point MCP at a **different worktree** | `make entra-run MCP_REPO=/absolute/path/to/other-checkout` | Override. Must be a directory. |
| Need certs + brokers + Entra SEMP PATCH, **not** the MCP process | `make entra-up` | No |
| Brokers stopped or profile drifted; keep the same containers | `make entra-up` again (converge, re-PATCH) | No |
| Brokers **exited** or wedged | `make entra-reset` then `make entra` | No |
| Done for the day; keep certs | `make entra-down` | No |
| MCP TLS cert is wrong | `make entra-certs-clean` then `make entra-up` (or `entra`); restart Claude | As for entra |
| Need Claude mcp remove/add + launch line | `make entra-claude-cmd` | No |
| Prove this laptop can run the brokers | `make entra-up` | No |

`MCP_REPO` is an **absolute path** to a solace-broker-mcp checkout whose `go run ./cmd/server` loads this lab YAML. **Default is the repo you ran `make` from.** Override when the code you want to test lives in another worktree. A typo'd override fails at preflight (`MCP_REPO is not a directory`).

`make entra-up` never starts MCP (`go run` blocks the terminal). `make entra` = `entra-up` then `entra-run`.

`make entra-up` does **not** call Microsoft. It starts three brokers, installs the SEMP TLS cert, and PATCHes Entra issuer/JWKS/audience/group onto the oauth containers. This process only uses `mcp-entra-solace-c` as `local-basic` (`admin`/`admin`). SEMP HTTP on 28081/28082/28083 answering 200 with `admin`/`admin` means the laptop + Docker/Podman path works.

This YAML sets `mcp_client_auth.scopes_supported`. YAML decoding is strict, so `MCP_REPO` must point at a checkout that has that field; an older tree fails at config load.

Claude sign-in is Hop 1 (MCP validates an Entra access token). `get-broker-status` against `local-basic` is Hop 1 + basic SEMP. `prod-us` / `test-us` OBO is not in this YAML.

The lab Host is `mcp-lab.solacetest.com` while the listener is loopback. `make entra-run` sets `MCPGODEBUG=disablelocalhostprotection=1` for that mismatch only. That env is not a product setting.

**`entra-reset` vs `entra-up`:** `entra-up` starts existing containers and re-PATCHes. `entra-reset` deletes and recreates the three `mcp-entra-solace*` names. It does not delete certs.

## Prerequisites (once per laptop)

1. **Hosts** (needs admin). Own line, not glued to FortiClient:

   ```
   127.0.0.1 mcp-lab.solacetest.com
   ```

   Do not disable FortiClient. `make entra` / `entra-up` / `entra-run` preflight this.

2. **Secret.** `cp local/entra/.env.example local/entra/.env` and set `MCP_SERVER_CLIENT_SECRET` to the mcp-broker client secret. Ask a teammate; it is not in git. Preflight fails on a missing or empty `.env` before brokers start. This Hop 1 YAML does not interpolate the secret; the file still belongs here so the lab stays one laptop setup.

3. **Entra IDs.** Tenant, app, and group GUIDs are only in `local/entra/entra-ids.sh`. Setup, YAML render, and `entra-claude-cmd` read that file. If the tenant or registrations change, edit that file, then `make entra-up` and restart MCP. Do not copy GUIDs into scripts or the YAML template.

## First run

```
make entra
```

Or, to run MCP from another worktree:

```
make entra MCP_REPO=/absolute/path/to/other-solace-broker-mcp
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

| Container            | Host ports  | This checkout's MCP alias |
| -------------------- | ----------- | ------------------------- |
| `mcp-entra-solace`   | 28081/21943 | not in YAML (Hop 2 later) |
| `mcp-entra-solace-c` | 28082/21944 | local-basic, `admin`/`admin` |
| `mcp-entra-solace-b` | 28083/21945 | not in YAML (Hop 2 later) |

Do **not** run `solace-local-infra/brokers/setup-oauth-brokers.sh` on these names — it writes Keycloak issuer/JWKS.

## Generated files

Ignored: `local/entra/.local/` (certs, rendered YAML), `local/entra/.env`. Do not commit them.

Committed: `local/entra/entra-ids.sh` (tenant/app/group IDs, no secrets).
