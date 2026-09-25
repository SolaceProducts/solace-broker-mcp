# Changelog Style Guide

Guidelines for writing concise, user-focused CHANGELOG entries. This is the source of truth for the `/changelog` skill.

---

## Why this matters

v0.9.0's 47 CHANGELOG entries had a median of 286 words per bullet (max 1464 words). Entries included implementation rationale, design choices, and mechanics — information that belongs in commit messages and code comments, not in release notes. Users need to know *what changed and why it matters to them*, not *why we architected it that way*. Entries 5–10× longer than necessary made it impossible to skim.

This guide ensures future releases are skimmable: a user reading the notes can understand what changed and what action (if any) they need to take in under 5 minutes total.

**This also raises the previous limits.** The prior guidance (15–30 words typical,
~50-word ceiling) was missed by 5.7× at the median — a rule nobody honors is a
broken rule. The ~50-word target and 100-word ceiling below are a deliberate
relaxation to a realistic ceiling, paired with an explicit condense step
(`/changelog` Step 5) that actually enforces it, rather than an aspirational number
everyone ignores.

---

## Categories

Every entry falls under one of these categories, in this order:

| Category | When to use | Example |
|----------|-------------|---------|
| **Added** | New feature, tool, config field, flag, or user-visible API | "New `get-queue-metrics` tool to retrieve queue depth without polling." |
| **Changed** | Behavior change, improved performance, or a notable internal API change that downstream code depends on (state explicitly that on-the-wire behavior is unchanged) | "Write tools now report desired-state outcomes (already_exists/already_absent) instead of errors." |
| **Deprecated** | Feature/flag/tool still present but will be removed; log a warning | "The `--legacy-auth` flag is deprecated and will be removed in v1.0." |
| **Removed** | Feature/flag/tool deleted | "Removed the `--legacy-auth` flag." |
| **Fixed** | Bug fix affecting user- or operator-visible behavior | "Fixed YAML comment substitution that dropped content after certain bracket patterns." |
| **Security** | Security-relevant fix or advisory | "Patched credential logging vulnerability in audit records." |

---

## Length and structure

**Priority:** coherence first, length second. Every entry must read as a complete, understandable thought.

**Normal entries (Added/Changed/Fixed/Removed/Deprecated):**
- **Target:** ~50 words (~1 sentence) — a guideline, not a floor to write up to. A
  simple change described in 6–15 words is not under-length; pad nothing.
- **Maximum:** 100 words (~2 sentences) — hard ceiling, never exceed **for entries
  drafted under this guide**. This ceiling is prospective: it governs new bullets
  the `/changelog` skill drafts from here forward, not existing `CHANGELOG.md`
  content written under the old rules. `/changelog` never rewrites a pre-existing
  bullet (see its Step 5/6), so condensing any current `[Unreleased]` entries that
  exceed 100 words is a separate, one-time cleanup outside this skill's scope.
- **Form:** one list bullet per logical change
- **What to include:** what changed, what users need to do (if action required)
- **What to exclude:** why it was built this way, how it works internally, design rationale

**Breaking changes:**
- **Target:** ~50 words (~1 sentence)
- **Maximum:** 100 words (~2 sentences, or a short migration table)
- **Prefix:** `- **BREAKING**: `
- **Include:** old behavior, new behavior, required migration action
- **Example:** "**BREAKING**: Queue subscription API renamed; migrate by calling `create-queue-subscription` instead of `add-subscription`. Tracked under SOL-12345." (15 words)

---

## Content standards

### Audience

You are writing for operators and tool consumers, not engineers. They read this to answer:
- *What do I need to change in my config?*
- *Do I need to update my queries or automation?*
- *Will this break my setup?*
- *What's the benefit to me?*

They do **not** read this to learn how the feature was designed. That belongs in:
- **Commit messages** — the "why" (rationale, trade-offs, decisions)
- **Code comments** — the "how" (mechanics, edge cases, implementation notes)
- **Architecture docs** — the big picture

### What to cut

If an entry contains any of these, condense or remove it:

| What | Where it belongs instead | Example to cut |
|------|-------------------------|-----------------|
| Implementation detail | Code comment | "We now use async iterators instead of channels internally..." |
| Design rationale | Commit message | "...because it reduces memory allocations in the hot path..." |
| Edge cases | Code comment | "This only applies when X and Y are both set..." |
| Historical context | Commit message | "We tried approach A first but found it broke..." |

### Ticket linkage

Every entry backed by a Jira ticket ends with:
```
Tracked under SOL-XXXXX.
```

Or, if multiple tickets:
```
Tracked under SOL-AAAAA and SOL-BBBBB.
```

Single-line housekeeping items (version bumps, license updates) carry no trailer.

---

## Examples

### Good

✅ "Added `list-queue-subscriptions` tool. Tracked under SOL-152847." (6 words)

✅ "Fixed YAML comment substitution that dropped content after certain bracket patterns. Tracked under SOL-153079." (14 words)

✅ "**BREAKING**: Config key `auth.username` renamed to `auth.identity`; update your broker config and re-deploy. Tracked under SOL-150123." (16 words)

These are simple, single-fact changes — most real entries will run shorter than the
~50-word target. A change with more moving parts (several affected callers, a
migration with more than one step) can reasonably use more of the range, up to the
100-word ceiling; ~50 is where most entries land, not a length to write up to.

### Too long (what to avoid)

❌ "We now expose tracing at every layer of the request path via OpenTelemetry, which means a failed tool call reads as one trace instead of scattered log lines across different applications. This required us to set the global text map propagator (previously a no-op default, which meant inbound W3C traceparent headers were silently discarded), and to carefully place the tracing middleware outside cross-origin protection but inside correlation so the correlation ID is already present when we stamp the span. Each layer uses its own named tracer so a backend can attribute spans to their source rather than to one server-wide scope. Tracked under SOL-152421." (104 words — **exceeds 100-word hard ceiling**)

**Condense to:**

✅ "Traces now span every layer of the request path, so a failed tool call reads as one trace instead of scattered logs. W3C traceparent headers are now propagated. Tracked under SOL-152421." (31 words)

---

## Breaking changes and migrations

Every **BREAKING** entry needs a migration path. Choose one form per change:

### Single action → inline sentence

```
- **BREAKING**: `X` renamed to `Y`. Update your config: change key `old` to `new` and restart. Tracked under SOL-12345.
```

### Multiple mappings → table

```
- **BREAKING**: Config keys consolidated. Migrate as follows:

| Old | New |
|-----|-----|
| `auth.username` | `auth.identity` |
| `auth.password` | `auth.secret` |
| `connection.timeout` | `connection.request_timeout` |

Tracked under SOL-12345.
```

Do not use a table for a single mapping; do not cram a multi-case migration into one sentence.

---

## Checklist

Before submitting a CHANGELOG entry (or after the `/changelog` skill drafts one):

- [ ] Category is one of: Added, Changed, Deprecated, Removed, Fixed, Security
- [ ] Bullet is under 100 words (target ~50, flex to 100 if needed for coherence)
- [ ] Entry states *what* changed and *why users care*, not *why we built it this way*
- [ ] No implementation detail, design rationale, or edge-case description
- [ ] Every identifier (tool name, config key, flag) exists as written in the codebase (grep to verify)
- [ ] If breaking: includes old behavior, new behavior, and migration action
- [ ] Jira ticket linkage present (`Tracked under SOL-XXXXX.`) or justifiable housekeeping item (no ticket needed)
- [ ] All changes in the branch diff are either covered by an entry or deliberately out of scope (test-only, internal API, etc.)
