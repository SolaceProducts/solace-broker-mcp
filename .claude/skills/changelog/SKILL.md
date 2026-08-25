---
name: changelog
description: Draft a Keep a Changelog entry for the current change in the house voice — correct category (incl. Deprecated/Security), BREAKING detection across config schema and tool names, and a migration note
user_invocable: true
---

# Changelog

Draft the `[Unreleased]` entry for the change on the current branch, matching the
existing voice of `CHANGELOG.md`. Invoked with `/changelog`.

The skill drafts and prints a diff; it never commits. A human reviews the entry in
the PR like any other change.

## When to use

- Before opening or updating a PR that changes behavior, config schema, or a tool.
- Not needed for changes with no user-visible or operator-visible surface (pure
  test/refactor/docs) — those may carry a `no-changelog` label instead.

## Usage

- `/changelog` — draft an entry for the current branch's changes
- `/changelog <git-range>` — draft for an explicit range, e.g. `v0.5.0..HEAD` or a
  single ticket's commits (required when drafting from `main`, which has no branch diff)

## Steps

### Step 1: Determine the change set and gather tickets

- On a feature branch: `git diff main...HEAD` for the file changes **and**
  `git log main..HEAD --format=%s%n%b` for commit subjects/bodies — the commits are
  the primary source of the SOL ticket(s) and the "why."
- With an explicit range argument: use it verbatim for both the diff and the log.
- If HEAD is on the default branch (`main`) and no range was given: **stop and ask
  for a range** — do not diff `main...HEAD` (it is empty and would silently no-op).
- PR context is optional: `gh pr view --json title,body` (or `gh pr list --search
  <SOL-ticket>` for a range) adds the "why." If `gh` is missing, unauthenticated, or
  the branch has no PR, continue from git data alone.
- Ticket sourcing: take `SOL-XXXXX` from the branch name, commit subjects, or PR
  title. If none can be found, write `Tracked under SOL-????.` and flag it for the
  human — **never invent a ticket number.**

### Step 2: Learn the house voice

Read `CHANGELOG.md` before drafting: the `[Unreleased]` section for what is
pending, and the recent version blocks for the fullest voice examples — the detailed
breaking-change paragraphs in `[0.5.0]` and `[0.3.0]`, and the migration **tables** in
`[0.3.0]`. Match what you see, do not impose a generic style:

- Keep a Changelog categories, in this order: **Added**, **Changed**, **Deprecated**,
  **Removed**, **Fixed**, **Security**. Added/Changed/Removed/Fixed are the common
  ones; use Deprecated and Security when the change is genuinely one (see Step 3).
- Each entry is **one list item (bullet) per logical change**, and most are one or
  two sentences. Length scales with impact, but the bar for a full paragraph is
  high: it is for a breaking change that genuinely needs old behavior, new behavior,
  and a migration path. Calibrate against the file as a whole — a typical entry is
  far shorter than the longest ones, which are outliers rather than the voice.
- Say what changed and what breaks. **Why** it changed belongs in the commit message,
  and **how** it works belongs in a code comment; an entry that runs long is usually
  carrying one of those. A reader of this file is updating their queries, dashboards,
  or config — not learning the design.
- Breaking changes are prefixed `- **BREAKING**: `. `**BREAKING**` is orthogonal to
  the category — a breaking change still files under Changed/Removed/etc. by its
  nature, and signals a SemVer MAJOR bump for the release/version decision.
- Ticket-backed entries end with `Tracked under SOL-XXXXX.` (or `... SOL-A and
  SOL-B.`). Trivial housekeeping items (license, badges, templates) carry no trailer,
  matching the existing one-line Added items.

### Step 3: Decide inclusion, classify, detect breaking

**Inclusion** — include a change only if it touches a published contract (config
schema, tool name/shape, HTTP endpoint, operator-visible behavior) **or** a notable
internal API that downstream code depends on. Exclude pure test/refactor/docs churn.
**If nothing qualifies, report that and exit without editing `CHANGELOG.md`.**

**Category** — beyond Added/Changed/Fixed:
- Announcing a feature/flag/tool will be removed — even if still nominally present
  (e.g. a flag now ignored that logs a deprecation warning) — files under
  **Deprecated**, not Changed.
- Actually removing it files under **Removed**.
- A security-relevant fix files under **Security**, not Fixed.
- A change to an unexported / `internal/` Go API with no on-the-wire effect files
  under **Changed** — and say so explicitly (e.g. "internal Go API; on-the-wire MCP
  schemas and operator config are unchanged").

**Breaking surfaces** — a change is BREAKING if it alters a contract clients or
operators depend on. Check **both**; config alone misses half of them:
- **Config schema** — `internal/config/`. New required fields, renamed/removed keys,
  stricter validation that rejects previously-accepted configs.
- **Tool names and shapes** — `internal/composite/definitions/tools.yaml` and native
  tool packages under `internal/tools/` (e.g. `sempv1/`, `queuemetrics/`) — any
  package that sets a `toolName` const / `Name:` field; registration in
  `internal/tools/register.go`. A renamed or removed tool is breaking (clients invoke
  tools by name). Tool names are **kebab-case** — never emit `snake_case`/`camelCase`.

**Grouping** — group by logical change, not by commit: several commits implementing
one feature produce one entry. Within a category, order BREAKING items first.

### Step 4: Write the migration note (breaking only)

Every BREAKING entry needs a migration note. Choose the form the house style uses,
by shape:

- **≥ 2 old→new mappings → a markdown table**, header `| Old ... | New ... |`
  (see the `client_auth` consolidation and broker-alias entries).
- **A single action → an inline `Migration: ...` prose sentence** (see the
  `allow_insecure_broker_tls` and `get-vpn-status` entries).

Do not emit a table for a one-line migration, and do not cram a multi-case migration
into one sentence.

### Step 5: Insert and show

- Insert with a **targeted Edit that adds only the new bullets** under the correct
  subsection, creating the subsection heading (in the Step 2 order) only if absent.
  Do not re-emit or rewrite any existing bullet — after editing, confirm the diff
  shows only additions.
- Write only `CHANGELOG.md`. Do not touch the `## Links` block or promote/date
  version blocks — that is the release process's job (today it is the manual
  `## Release Process` in `CHANGELOG.md` / `RELEASING.md`; automating the
  release-notes link is tracked separately). Do not commit, push, or tag.
- Print the resulting diff and stop for human review.

### Step 6: Check the draft against the tree

Two checks, both against the code rather than against what the change was meant to
do. They exist because the common failure is describing the design as intended
rather than as shipped, after a later revision moved it.

- **Every identifier the entry names must exist as written** — log message, field,
  config key, flag, tool name. Grep each one. A name that no longer exists sends a
  reader looking for something that was renamed mid-branch.
- **Anything observable in the diff that the entry omits.** Re-read the diff for
  changed strings and fields and confirm each is either covered or deliberately out
  of scope. Where an entry claims a category — "the old messages are replaced" —
  list **all** of them: a partial list reads as complete, which is worse than a
  vague sentence.

## Rules

- Never place secrets, tokens, or credentials in an example.
- Never invent a SOL ticket number — use the `SOL-????.` placeholder and flag it.
- One bullet per logical change; omit pure test/refactor/docs churn.
- An entry describes the branch's whole diff against `main`, not any single commit.
  If commits land after the entry is drafted, **re-derive from `main..HEAD` and
  rewrite** rather than patching the existing text — patching assumes you already
  know what those commits changed, which is the assumption Step 1 exists to avoid.
- If nothing user- or operator-visible changed, say so and write nothing.
