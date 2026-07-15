---
name: dax-onboard
description: Interactive onboarding for new DAX (Developer and Agent Experience) team members. Walks through team context, environment setup, architecture, workflows, and first-ticket guidance, adapting depth to the joiner's role and experience. Use for onboarding, or invoke a specific phase with `/dax-onboard setup | architecture | first-ticket`.
---

# DAX (Developer and Agent Experience) — Team Onboarding Skill

## Usage

`/dax-onboard [command]`

Commands:
- (none) — Full onboarding, all phases
- `setup` — Install prerequisites, clone repo, configure and run the MCP server
- `architecture` — Domain concepts, design docs, and architecture walkthrough
- `first-ticket` — Find a good starter ticket from the backlog

---

## Skill Instructions

**Direct phase access:** If invoked with an argument (e.g. `/dax-onboard setup`), jump
directly to the matching phase:
- `setup` → Phase 3 (Repository & Environment Setup)
- `architecture` → Phase 4 (Architecture & Domain Knowledge)
- `first-ticket` → Phase 6 (First Contribution)

If no argument is given, follow all phases in order.

Use `AskUserQuestion` at decision points. Pull live data from Confluence/Jira where
page IDs are provided rather than relying on the static text in this file.

---

### Phase 1: Assess the New Joiner

Before doing anything else, ask these questions to tailor the rest of the onboarding:

**Question 1 — Role**
> What's your role on the team?

Options:
- Software developer — works across any DAX product/area (server code, tooling, tests, infra, etc.)
- Software architect
- Product manager
- Engineering manager
- Other

**Question 2 — Product Area**
> Which product area will you be working on first?

Options:
- Broker MCP Server (`MCPBroker`) — AI-driven broker monitoring and management
- Mesh MCP Server (`MCPMesh`) — fleet-wide event mesh visibility
- Not sure yet / all of them

**Question 3 — Experience baseline**
> Rate your familiarity with each (none / some / strong):

- Go
- MCP (Model Context Protocol)
- Solace event brokers / SEMP API
- OAuth 2.0 / OIDC / JWT

Store the answers — they control which sections get expanded vs. summarized.

---

### Phase 2: Team Briefing

Fetch the team home page from Confluence and build the briefing from it. **Do not rely on
hardcoded team facts** — products, ownership, and channels drift. Treat the live page as the
source of truth; the notes below only say *what to extract*, not *what the answer is*.

**Confluence source:**
- Page ID: `6351454348` (DAX Home)
- Space: `DAX`

**Cover these points:**

#### 2a. Mission
Pull the mission statement from the DAX Home page (currently framed around DAX =
"Developer and Agent Experience"). Quote it as written on the page.

#### 2b. What the team owns
Read the products/components table on the DAX Home page and present each product with what
it does, its current status/milestone, and its owning Slack channel. The page is the
authoritative list — include whatever products it currently names (e.g. Broker MCP, Docs MCP,
Mesh MCP, IaC providers) rather than a fixed set, and carry over any "future / not yet owned"
qualifiers exactly as the page states them. Note the linked milestone/epic Jira keys.

#### 2c. People

| Name | Role / Focus |
|------|-------------|
| Andrea Ross | Senior Engineering Manager and Product Owner|
| Balazs Czoma | Senior Architect |
| Matthew Diotte | Senior Technical Product Manager |
| Amit Morade | Software Developer |
| Asad Waheed | Software Developer |
| Lootii Kiri | Software Developer |
| Wajiha Maryam | Software Developer |

#### 2d. Communication channels
Extract the Slack channels from the DAX Home page's products table (each product row lists its
channel) plus the squad channel. Do not hardcode a channel list.

#### 2e. Current priorities

Fetch live from Jira (backlog board: `7637`, project `SOL`).

1. **In-flight work** — top 3-5 non-done items by priority/recent activity.
2. **Open epics** — list the open/in-progress Epics to show the larger workstreams:
   `project = SOL AND issuetype = Epic AND statusCategory != Done ORDER BY updated DESC`
   Summarize each epic in a line so the joiner sees the themes, not just individual tickets.

#### 2f. Broker MCP Launch Roadmap

**Confluence source:** Page ID `6345359372` (in Andrea Ross's personal space)

This page has the full launch plan with dates, scope per phase, and open concerns.
Fetch it live and present the current timeline, success metrics, and strategic context.

---

### Phase 3: Repository & Environment Setup

Actively guide the joiner through setup, running commands with confirmation at each step.
If invoked directly via `/dax-onboard setup`, ask which product area first.

#### Repo Map

| Product Area | Repo | Purpose |
|-------------|------|---------|
| Broker MCP | `SolaceDev/solace-broker-mcp` | MCP server source code |
| All MCP | `SolaceDev/discovery/Broker-MCP` | Design documents (subdirs per product) |

#### Automated Setup: Broker MCP

Run each step below, confirming with the user before proceeding. Stop and troubleshoot
if any step fails.

**Step 1 — Check and install prerequisites**
- Check Go: `go version` (needs 1.25+)
  - If missing or outdated: `brew install go` (macOS) or `sudo snap install go --classic` (Linux)
- Check Docker: `docker --version`
  - If missing: `brew install --cask docker` (macOS) or guide through [Docker install](https://docs.docker.com/get-docker/) (Linux)
- Confirm both are available before proceeding

**Step 2 — Clone the repo**
```bash
git clone git@github.com:SolaceDev/solace-broker-mcp.git
cd solace-broker-mcp
go mod download
```
> **SSO note:** `SolaceDev` enforces SAML SSO. The SSH clone above fails with an SSO error
> unless your SSH key is authorized for the org. If it does, use the GitHub CLI fallback
> (works over HTTPS with an SSO-authorized token — check with `gh auth status`):
> ```bash
> gh repo clone SolaceDev/solace-broker-mcp
> cd solace-broker-mcp
> go mod download
> ```
> If SSH is preferred, tell the user to authorize their SSH key for the org first:
> go to GitHub → Settings → SSH and GPG keys → find the key → "Configure SSO" → authorize
> for `SolaceDev`, then retry the SSH clone.

**Step 3 — Create local config**
- Copy example config: `cp broker-config.example.yaml broker-config.yaml`
- Create `.env` file with default dev credentials:
  ```env
  BROKER_USERNAME=admin
  BROKER_PASSWORD=admin
  ```
- Ask the user if they have a broker URL to configure, or if they want to use Docker (Step 4)

**Step 4 — Start a local Solace broker (if needed)**
```bash
docker run -d --name solace-broker \
  -p 8080:8080 -p 55555:55555 \
  -e username_admin_globalaccesslevel=admin \
  -e username_admin_password=admin \
  solace/solace-pubsub-standard:latest
```
- Wait for broker health: `docker logs solace-broker` until "Running" appears
- Update `broker-config.yaml` to point at `http://localhost:8080`

**Step 5 — Build and run**
```bash
go run ./cmd/server
```
- Verify health endpoint: `curl http://localhost:9090/health` → `{"status": "ok"}`

**Step 6 — Connect from Claude Code**
```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```
- Test with a query: "List brokers" should return the configured alias

**Step 7 — Run tests**
```bash
go test ./...
```
- Confirm tests pass. If E2E tests fail due to missing broker, that's expected without
  the full test environment — unit tests should all pass.

#### Mesh MCP

- Early stage — code repo coming later in Q1
- Design docs in `SolaceDev/discovery/Mesh-MCP/`

---

### Phase 4: Architecture & Domain Knowledge

Adapt depth based on Phase 1 answers. For each product area, there's a "concepts"
section (always shown) and a "deep dive" section (shown if the joiner is working
in that area or asks for it).

> **Source concrete specifics live from the repo.** This section deliberately avoids
> hardcoding fast-moving facts (how many tools exist, the composite-vs-native split, the
> exact `internal/` layout). Pull those from the repo's own authoritative docs at runtime —
> `README.md`, `TOOLS.md` / `docs/tools-reference.md`, and the real `internal/` tree — via the
> local clone or `gh api repos/SolaceDev/solace-broker-mcp/contents/<path>`, and present the
> current values. The concept *definitions* below are stable and safe to use as written.

#### 4a. Concepts (always cover)

| Concept | One-line explanation | Learn more |
|---------|---------------------|------------|
| MCP (Model Context Protocol) | Standard for AI agents to call tools exposed by servers | [MCP spec](https://modelcontextprotocol.io) |
| SEMP API | Solace's management API for broker configuration and monitoring | [SEMP overview](https://docs.solace.com/Admin/SEMP/Using-SEMP.htm) |
| SEMPv1 vs SEMPv2 | Two different broker APIs — SEMPv1 is legacy XML, SEMPv2 is modern REST/JSON; the codebase uses both | `internal/semp/` in repo |
| Event mesh | Network of interconnected Solace brokers | [Event mesh overview](https://docs.solace.com/Get-Started/event-mesh-intro.htm) |
| Composite tools | YAML-driven multi-step tools that orchestrate SEMPv2 calls — most MCP tools are defined this way rather than as native Go handlers (get the current split from `TOOLS.md` / the repo) | `internal/composite/definitions/tools.yaml` in repo |
| Broker alias | Config-defined name for a broker that clients pass as the `broker` parameter in every tool call | `internal/config/` in repo |
| Token exchange (RFC 8693) | Swapping an agent's token for a broker-scoped token | Confluence: Solace Brokers, OAuth, and Token Exchange (page 6288277527) |
| RFC 9728 (Protected Resource Metadata) | OAuth discovery mechanism — how MCP clients find which auth server to use | `/.well-known/oauth-protected-resource` endpoint |
| Scope-based RBAC | Authorization layer controlling which MCP tools a user can invoke | Confluence: Scope-Based RBAC (page 6410240001) |

#### 4b. Design Document Index

> The list below is a convenience starting set and **will drift** as docs are added/moved.
> Treat it as pointers, not the source of truth: confirm titles/IDs are still valid when you
> fetch, and discover current docs live — search the Confluence space or read the child pages
> of the RBAC hub (`6410240001`) — rather than assuming this table is complete.

| Document | Confluence ID | Read when... |
|----------|--------------|-------------|
| Scope-Based RBAC (hub page) | `6410240001` | Working on auth, permissions, or token handling |
| Design Thinking and Operator Model | `6410240030` | Understanding *why* the RBAC design looks the way it does |
| RBAC Architecture | `6410240051` | About to write RBAC code |
| Components and Subtasks | `6410240114` | Breaking down RBAC implementation work |
| Keycloak Test Baseline | `6410240072` | Setting up OAuth test environment |
| Keycloak Setup Walkthrough | `6410240093` | Click-by-click Keycloak setup |
| Prototype Notes | `6410240135` | Understanding edge cases found during prototyping |
| Solace Brokers, OAuth, and Token Exchange | `6288277527` | Understanding how token exchange works with Solace broker OAuth modes |
| Token Exchange Cache Design | `6441271318` | Working on token caching, renewal, or observability |
| Quality Plan | `6409683254` | Defining test strategy |
| Broker MCP Tool/Prompt Matrix | `6349455466` | Understanding cross-model compatibility |
| Broker MCP Launch Plans and Concerns | `6345359372` | Understanding launch phases, dates, strategic positioning, and open exec-level concerns |

When the joiner selects a product area, fetch and summarize the top 2-3 most relevant
docs at a depth appropriate to their experience level:
- **None/some familiarity:** Explain the problem being solved, the key decision, and
  the outcome. Skip implementation details.
- **Strong familiarity:** Focus on decisions, trade-offs, and anything surprising or
  non-obvious.

#### 4c. Architecture Walkthrough

For each product area, read the repo's own documentation and present a walkthrough
tailored to the joiner's experience level.

**Broker MCP Server:**
- Repo: `SolaceDev/solace-broker-mcp`
- Read `docs/` directory for architecture, authentication, and secure logging docs
- Walk through the `internal/` package structure to explain key components

> **If the repo isn't cloned locally** (e.g. the joiner ran `/dax-onboard architecture`
> without doing setup first), don't assume a working copy exists. Read the files straight
> from GitHub via the SSO-authorized `gh` CLI — no clone needed:
> ```bash
> gh api repos/SolaceDev/solace-broker-mcp/contents/docs        # list docs/
> gh api repos/SolaceDev/solace-broker-mcp/contents/docs/<file> --jq '.content' | base64 -d
> ```
> Useful targets: `docs/`, `TOOLS.md`, `CLAUDE.md`, `internal/composite/definitions/tools.yaml`.
> For heavier browsing, a shallow sparse clone (`git clone --depth 1` + sparse-checkout of
> `docs/ internal/`) also works. Confluence design docs (4b) need no repo at all.

Cover these points at a level matching the joiner's familiarity:
- How a request flows from AI agent → MCP server → Solace broker and back
- The two-hop auth model (client→server via OAuth JWT, server→broker via basic/bearer)
- How tools are defined (YAML-driven composite tools vs native Go handlers)
- External integrations (SEMPv2 REST API, SEMPv1 XML API, OAuth/OIDC IdP)

---

### Phase 5: Workflow & Conventions

#### How work gets planned

| Aspect | Convention |
|--------|-----------|
| Jira project | `SOL` |
| Board | `7637` |
| Components | In use, per product area — e.g. `MCPBroker` (expect `MCPDocs`, `MCPMesh` as they ramp) |
| Sprint cadence | Biweekly |
| Backlog grooming | As needed |
| Branch strategy | Trunk-based development |

---

### Phase 6: First Contribution

After all previous phases, help the joiner pick their first ticket.

**Steps:**
1. Fetch open issues from the backlog (board `7637`) with JQL:
   ```
   project = SOL AND status in (Open, "To Do") AND component = [selected_area]
   ORDER BY priority DESC, created ASC
   ```
2. Filter for good starter criteria:
   - Has a clear description
   - Low blast radius (doesn't touch auth, core protocol, or release infra)
   - Ideally has less than 3 story points. **Story Points live in `customfield_10300`** on this
     Jira instance — request it explicitly in the search `fields`. If a ticket has no value in
     `customfield_10300`, treat it as unestimated, not as small.
3. Present top 3 options with:
   - Ticket summary and link
   - Which files/packages are likely involved
   - Relevant design docs
   - Estimated complexity (small / medium)
4. Offer to walk through the relevant code paths for the chosen ticket
