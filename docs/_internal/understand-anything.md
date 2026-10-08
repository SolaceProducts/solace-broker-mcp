# Understand Anything — quick start

We use [Understand Anything](https://github.com/Egonex-AI/Understand-Anything) to
explore this codebase as an interactive knowledge graph: every file, function, and
type as a node you can search, click, and read plain-English summaries for.

**The graph is committed to this repo (`.ua/`).** Most people never run the analysis —
you just view what's already there. Only pick the second path if you want to
regenerate or update the graph.

| I want to... | Path | Needs |
|---|---|---|
| **View** the committed graph | Tier 1 below | Node ≥ 18 only |
| **Regenerate / update** the graph | Tier 2 below | Claude Code + Node ≥ 18 + pnpm |

---

## Tier 1 — View the graph (most people)

No Claude Code, no LLM, no API key. Everything is served read-only from local disk;
nothing leaves your machine.

**Prerequisite:** Node.js ≥ 18 ([nodejs.org](https://nodejs.org), or `brew install node`).

**Run from the repo root:**

```bash
npx https://github.com/Egonex-AI/Understand-Anything/releases/latest/download/understand-anything-viewer.tgz .
```

The terminal prints a tokenized local URL (`http://127.0.0.1:5173/?token=…`) and opens
the dashboard in your browser. Pan, zoom, search, click any node for its code and a
summary. Stop with `Ctrl-C`.

That's it. If the dashboard is empty, make sure you ran the command from the repo root
(the directory that contains `.ua/`) and that you've pulled the latest `main`.

---

## Tier 2 — Regenerate or update the graph (maintainers)

Run this when the code has drifted from the graph and you want to refresh it. The first
full run is token-heavy; later runs are incremental (only changed files).

**Prerequisites**

- **Claude Code** (or another supported agent — Cursor, Copilot, Codex, Gemini CLI).
- **Node.js ≥ 18.**
- **pnpm** — `npm install -g pnpm` (Node 26 no longer bundles Corepack).

**Install the plugin (Claude Code)**

```
/plugin marketplace add Egonex-AI/Understand-Anything
/plugin install understand-anything@understand-anything
```

Then `/reload-plugins` (or restart Claude Code) so the `/understand` skill loads.

> The first `/understand` run installs and builds the plugin's own Node dependencies
> inside the plugin directory — that's why pnpm is required and why the first run pauses
> for a minute before analyzing. Subsequent runs skip this.

**Generate / update the graph**

```
/understand                 # incremental by default; only re-analyzes changed files
/understand --full          # force a full rebuild
/understand --auto-update   # install a post-commit hook that keeps the graph fresh
```

**Explore and ask questions**

```
/understand-dashboard                 # open the interactive dashboard
/understand-chat How does auth work?  # ask about the codebase
/understand-explain internal/tools/register.go
/understand-onboard                   # generate an onboarding guide
```

### Two things to know before you run it

- **Token cost.** The initial full analysis reads the whole repo and consumes a
  meaningful number of tokens. Re-runs are incremental and far cheaper.
- **Where your code goes.** The semantic layer sends source to whatever model your
  agent uses (for Claude Code, that's Anthropic — the same trust boundary you already
  accept by using Claude Code on this repo). The tool itself is local: no telemetry, no
  phone-home. For a more sensitive repo you can point the agent at a local model
  (for example, Ollama).

---

## What's committed, and what isn't

Commit everything in `.ua/` **except** the local scratch files, which are gitignored:

```gitignore
.ua/intermediate/
.ua/diff-overlay.json
```

The committed graph is generated data (LLM summaries of our own code), not third-party
source — no license entanglement with our Apache 2.0 licensing. Treat it as
source-equivalent for confidentiality, since it describes the codebase.

## Reference

- Upstream: <https://github.com/Egonex-AI/Understand-Anything>
- License: MIT (the tool). It runs alongside this repo; it is not linked into or shipped
  with our binary.
- Our architecture overview lives in [`architecture.md`](architecture.md); the graph
  complements it, it doesn't replace it.
