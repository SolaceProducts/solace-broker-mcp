# Third-Party Dependency Classification

This is the compliance classification record for the dependencies compiled
into the `solace-broker-mcp` binary — the same closure `THIRD_PARTY_LICENSES.md`
inventories, one row per line in that file, in the same order. Where that file
answers "what licence does each dependency carry," this file answers "what did
Solace's third-party policy classify each one as, and who approved the ones
that needed a human decision."

**Source: FOSSA project `SolaceProducts_solace-broker-mcp`**, checked directly
against its Issues, Licenses, and Dependencies views rather than inferred from
licence type alone — **2026-08-26**. Re-verify at
`https://app.fossa.com/projects/custom+48578/SolaceProducts_solace-broker-mcp`
before trusting this table for a release cut after that date; it is a snapshot,
not a live query.

## Zero red-classified dependencies

Stated explicitly, not by omission: **no dependency in this closure is
classified red.** FOSSA's Issues tab for this project reports "No issues here"
— zero active policy violations across all 29 modules — and every licence
detected across the whole dependency tree (Apache-2.0, MIT, BSD-3-Clause,
MPL-2.0, ISC, plus incidental Creative-Commons-licensed sample test data
attributable to `golang.org/x/text` and already tracked in
`THIRD_PARTY_LICENSES.md`'s Notes section) is either permissive or the one
weak-copyleft licence classified yellow below. No strong-copyleft licence
(GPL, LGPL, AGPL, EPL, CDDL) appears anywhere in the closure.

## Yellow-classified dependencies — require a named approver

Weak (file-level) copyleft. Compatible with this project's Apache-2.0
distribution when used unmodified, per the terms `THIRD_PARTY_LICENSES.md`'s
own weak-copyleft section already documents, but classified yellow rather than
green because that compatibility is a per-dependency judgement call, not a
blanket fact about the licence.

No version column — classification is a per-module policy decision, not a
per-version fact, and a table implying otherwise would read as stale the next
time either dependency takes a version-only bump (`THIRD_PARTY_LICENSES.md`
tracks the version; this file tracks the verdict).

| Component | License | Approved by | Date |
|---|---|---|---|
| `github.com/hashicorp/go-cleanhttp` | MPL-2.0 | Team decision, ratification pending — Andrea Ross | 2026-08-26 |
| `github.com/hashicorp/go-retryablehttp` | MPL-2.0 | Team decision, ratification pending — Andrea Ross | 2026-08-26 |

The verdict itself (weak copyleft, unmodified use, compatible) is not in
question — this status is about who has formally signed off, not whether the
classification is right. Update this table to a plain named approval once
Andrea Ross has actually confirmed it; until then, this reflects the basis the
team proceeded on, not an obtained personal sign-off.

## Green-classified dependencies

Permissive licences — Apache-2.0, MIT, BSD-3-Clause, ISC. No policy conflict,
no approval required.

| Component | License |
|---|---|
| `github.com/coreos/go-oidc/v3/oidc` | Apache-2.0 |
| `github.com/davecgh/go-spew/spew` | ISC |
| `github.com/getkin/kin-openapi` | MIT |
| `github.com/go-jose/go-jose/v4` | Apache-2.0 |
| `github.com/go-jose/go-jose/v4/json` | BSD-3-Clause |
| `github.com/go-openapi/jsonpointer` | Apache-2.0 |
| `github.com/go-openapi/swag/jsonname` | Apache-2.0 |
| `github.com/google/jsonschema-go/jsonschema` | MIT |
| `github.com/maypok86/otter/v2` | Apache-2.0 |
| `github.com/modelcontextprotocol/go-sdk` | Apache-2.0 |
| `github.com/oasdiff/yaml` | MIT |
| `github.com/oasdiff/yaml3` | MIT |
| `github.com/pmezard/go-difflib/difflib` | BSD-3-Clause |
| `github.com/santhosh-tekuri/jsonschema/v6` | Apache-2.0 |
| `github.com/segmentio/asm` | MIT |
| `github.com/segmentio/encoding` | MIT |
| `github.com/sony/gobreaker/v2` | MIT |
| `github.com/stretchr/testify` | MIT |
| `github.com/xeipuuv/gojsonpointer` | Apache-2.0 |
| `github.com/xeipuuv/gojsonreference` | Apache-2.0 |
| `github.com/xeipuuv/gojsonschema` | Apache-2.0 |
| `github.com/yosida95/uritemplate/v3` | BSD-3-Clause |
| `golang.org/x/oauth2` | BSD-3-Clause |
| `golang.org/x/sync` | BSD-3-Clause |
| `golang.org/x/sys/cpu` | BSD-3-Clause |
| `golang.org/x/text` | BSD-3-Clause |
| `golang.org/x/time/rate` | BSD-3-Clause |
| `gopkg.in/yaml.v3` | MIT |

## What "red" would mean, and why FOSSA is the source rather than licence type alone

A red classification is a strong-copyleft licence (GPL/LGPL/AGPL/EPL/CDDL) or
any dependency FOSSA's policy engine flags as `policy_conflict` or
`unlicensed_dependency` — the same two conditions `guardian-scan.yaml`'s
Guardian gate already blocks a release on. This record does not re-derive that
verdict from licence text; it's sourced from FOSSA's actual policy result
because FOSSA's configured policy is the thing that's authoritative for what
counts as a conflict, not general OSS-licence convention. Concretely: every
dependency's classification above was cross-checked against FOSSA's
Dependencies-tab verdict for that exact module and version, not inferred.

## Keeping this current

This is a **committed snapshot, not a generated artifact** — unlike the SBOM
(regenerated fresh every release) or `THIRD_PARTY_LICENSES.md` (kept in sync
by `.github/scripts/licenses-check.sh` and `make refresh-third-party-inventory`
on every dependency change), this file only changes when a dependency's
licence classification actually changes, because a yellow approval is a human
decision that must survive a re-run, not something to reconstruct at release
time. See `RELEASING.md`'s **SBOM and dependency classification** section
for what a maintainer does when a new dependency lands.
