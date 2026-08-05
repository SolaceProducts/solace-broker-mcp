# Guardian Scan (composite action)

Runs the full supply-chain + container scan for this repo and reports the results
to [Guardian](https://guardian.solacedev.ca), Solace's vulnerability aggregation
service. One code path serves pull requests, pushes to `main`, and release
readiness — the only difference is the `mode` input.

It wraps the reusable actions published in
[`SolaceDev/solace-public-workflows`](https://github.com/SolaceDev/solace-public-workflows)
(`sca/sca-scan`, `fossa-guard`, `prisma-cloud-scan`, `guardian-vulnerability-diff`,
`guardian-db-sync`, `guardian-vulnerability-gate`), all pinned by SHA.

## Why a composite action

The FOSSA / Guardian / Prisma credentials live in the **`guardian` GitHub
Environment**, because this is a public repository and HashiCorp Vault (internal
OIDC) must not be reached from it. A job that *calls a reusable workflow* cannot
declare `environment:`, so it cannot read an environment secret. A composite
action runs inside its caller's job, so the caller declares `environment: guardian`,
reads `${{ secrets.* }}`, and passes them in here. See `.github/ADMIN_SETUP.md`.

## What it does

1. Builds the Docker image locally (`linux/amd64`, `--load`, never pushed) so
   Prisma can scan it without a registry pull.
2. **FOSSA SCA** (`sca-scan`) — analyzes dependencies; uploads to Guardian in `trunk` mode.
3. **FOSSA licensing gate** — `BLOCK` on trunk, `REPORT`/diff on PR.
4. **FOSSA vulnerability guard** — `REPORT` in both modes; the Guardian gate is the
   SLA-aware vulnerability blocker (matches solace-agent-mesh-enterprise).
5. **Prisma Cloud scan** — never blocks locally; uploads to Guardian on trunk.
6. **PR:** `guardian-vulnerability-diff` posts a diff vs the deployed baseline
   (auto-suppressed on public repos, never blocks).
7. **Trunk:** `guardian-db-sync` (unifiers `fossa,prisma`) then
   `guardian-vulnerability-gate`, which **fails the job when Guardian blocks**.
   A final step also fails on a FOSSA licensing block.

## Inputs

| Input | Required | Default | Description |
|-------|----------|---------|-------------|
| `mode` | yes | — | `pr` (report/diff, no upload/gate) or `trunk` (upload + gate + block) |
| `fossa-api-key` | yes | — | FOSSA API key |
| `guardian-url` | yes | — | Guardian API base URL (a secret in this repo) |
| `guardian-token` | yes | — | Guardian API bearer token |
| `prisma-console-url` | yes | — | Prisma Cloud Console URL |
| `prisma-user` | yes | — | Prisma Cloud access key id |
| `prisma-pass` | yes | — | Prisma Cloud secret key |
| `product-name` | no | `solace-broker-mcp` | Guardian product name |
| `fossa-project-id` | no | `SolaceProducts_solace-broker-mcp` | FOSSA project id |
| `image-registry` | no | `ghcr.io` | Registry segment of the scanned image name |
| `image-repo` | no | `solacedev/solace-broker-mcp` | Repository segment of the scanned image name |
| `fossa-branch` | yes | — | FOSSA branch label (`main` on trunk, `PR` on pull requests) |
| `fossa-revision` | yes | — | FOSSA revision (sha, head ref, or release version) |
| `product-version` | no | `main` | Guardian product version path segment |
| `product-full-version` | yes | — | Guardian product full version (release tag or `<tag>-<sha>`) |
| `fail-on-blocked` | no | `true` | `false` for an admin-authorised release bypass |
| `github-token` | no | `${{ github.token }}` | Token for PR comments / status checks |

## Usage

The caller job **must** declare `environment: guardian` and check out the repo
first (the image build and FOSSA analysis run against the workspace).

```yaml
jobs:
  security-scan:
    runs-on: ubuntu-latest
    environment: guardian
    permissions:
      contents: read
      pull-requests: write
      checks: write
      statuses: write
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: 'go.mod'
      - uses: ./.github/actions/guardian-scan
        with:
          mode: trunk
          fossa-api-key: ${{ secrets.FOSSA_API_KEY }}
          guardian-url: ${{ secrets.GUARDIAN_URL }}
          guardian-token: ${{ secrets.GUARDIAN_API_TOKEN }}
          prisma-console-url: ${{ secrets.PRISMACLOUD_CONSOLE_URL }}
          prisma-user: ${{ secrets.PRISMACLOUD_ACCESS_KEY_ID }}
          prisma-pass: ${{ secrets.PRISMACLOUD_SECRET_KEY }}
          fossa-branch: main
          fossa-revision: ${{ github.sha }}
          product-full-version: ${{ github.sha }}
```
