# Third-Party Licenses

This file lists the third-party components compiled into the `solace-broker-mcp`
binary, with their versions and licenses. It is the human-readable OSS compliance
inventory that accompanies the release.

**Generated** 2026-07-31 with
[`go-licenses`](https://github.com/google/go-licenses) against `./cmd/server`:

```bash
go run github.com/google/go-licenses@v1.6.0 csv ./cmd/server
```

Regenerate on any `go.mod` change, not only before a release. Between 2026-07-17
and 2026-07-31 this file drifted from the binary in both directions, so
`.github/scripts/licenses-check.sh` now fails CI when the inventory stops
matching `go list -deps ./cmd/server`. The FOSSA scan remains the authoritative
automated check for licence *policy*; this file is the human-readable inventory.

All components are compatible with distribution under the Apache License 2.0.
No strong-copyleft licenses (GPL, LGPL, AGPL, EPL, CDDL) are linked into the
binary.

## Weak-copyleft components (MPL-2.0)

Two components are licensed under the Mozilla Public License 2.0. MPL-2.0 is
file-level copyleft and is compatible with an Apache 2.0 distribution. We use
them unmodified and preserve their license text and copyright headers. Their
source is publicly available at the repositories named in their module paths. If
we ever modify their source files, we must make those modified files available
under MPL-2.0.

| Component | Version | License | License text |
|---|---|---|---|
| `github.com/hashicorp/go-cleanhttp` | v0.5.2 | MPL-2.0 | [license](https://github.com/hashicorp/go-cleanhttp/blob/v0.5.2/LICENSE) |
| `github.com/hashicorp/go-retryablehttp` | v0.7.8 | MPL-2.0 | [license](https://github.com/hashicorp/go-retryablehttp/blob/v0.7.8/LICENSE) |

## Permissive components

| Component | Version | License | License text |
|---|---|---|---|
| `github.com/coreos/go-oidc/v3/oidc` | v3.18.0 | Apache-2.0 | [license](https://github.com/coreos/go-oidc/blob/v3.18.0/LICENSE) |
| `github.com/davecgh/go-spew/spew` | v1.1.1 | ISC | [license](https://github.com/davecgh/go-spew/blob/v1.1.1/LICENSE) |
| `github.com/getkin/kin-openapi` | v0.134.0 | MIT | [license](https://github.com/getkin/kin-openapi/blob/v0.134.0/LICENSE) |
| `github.com/go-jose/go-jose/v4` | v4.1.4 | Apache-2.0 | [license](https://github.com/go-jose/go-jose/blob/v4.1.4/LICENSE) |
| `github.com/go-jose/go-jose/v4/json` | v4.1.4 | BSD-3-Clause | [license](https://github.com/go-jose/go-jose/blob/v4.1.4/json/LICENSE) |
| `github.com/go-openapi/jsonpointer` | v0.21.0 | Apache-2.0 | [license](https://github.com/go-openapi/jsonpointer/blob/v0.21.0/LICENSE) |
| `github.com/go-openapi/swag` | v0.23.0 | Apache-2.0 | [license](https://github.com/go-openapi/swag/blob/v0.23.0/LICENSE) |
| `github.com/google/jsonschema-go/jsonschema` | v0.4.2 | MIT | [license](https://github.com/google/jsonschema-go/blob/v0.4.2/LICENSE) |
| `github.com/josharian/intern` | v1.0.0 | MIT | [license](https://github.com/josharian/intern/blob/v1.0.0/license.md) |
| `github.com/mailru/easyjson` | v0.7.7 | MIT | [license](https://github.com/mailru/easyjson/blob/v0.7.7/LICENSE) |
| `github.com/maypok86/otter/v2` | v2.3.0 | Apache-2.0 | [license](https://github.com/maypok86/otter/blob/v2.3.0/LICENSE) |
| `github.com/modelcontextprotocol/go-sdk` | v1.5.0 | Apache-2.0 | [license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.5.0/LICENSE) |
| `github.com/mohae/deepcopy` | c48cc78d4826 | MIT | [license](https://github.com/mohae/deepcopy/blob/c48cc78d4826/LICENSE) |
| `github.com/oasdiff/yaml` | a3ea61cb4d4c | MIT | [license](https://github.com/oasdiff/yaml/blob/a3ea61cb4d4c/LICENSE) |
| `github.com/oasdiff/yaml3` | 61cd415a242b | MIT | [license](https://github.com/oasdiff/yaml3/blob/61cd415a242b/LICENSE) |
| `github.com/perimeterx/marshmallow` | v1.1.5 | MIT | [license](https://github.com/perimeterx/marshmallow/blob/v1.1.5/LICENSE) |
| `github.com/pmezard/go-difflib/difflib` | v1.0.0 | BSD-3-Clause | [license](https://github.com/pmezard/go-difflib/blob/v1.0.0/LICENSE) |
| `github.com/segmentio/asm` | v1.1.3 | MIT | [license](https://github.com/segmentio/asm/blob/v1.1.3/LICENSE) |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | [license](https://github.com/segmentio/encoding/blob/v0.5.4/LICENSE) |
| `github.com/sony/gobreaker/v2` | v2.4.0 | MIT | [license](https://github.com/sony/gobreaker/blob/v2.4.0/LICENSE) |
| `github.com/stretchr/testify` | v1.11.1 | MIT | [license](https://github.com/stretchr/testify/blob/v1.11.1/LICENSE) |
| `github.com/woodsbury/decimal128` | v1.3.0 | 0BSD | [license](https://github.com/woodsbury/decimal128/blob/v1.3.0/LICENCE) |
| `github.com/xeipuuv/gojsonpointer` | 4e3ac2762d5f | Apache-2.0 | [license](https://github.com/xeipuuv/gojsonpointer/blob/4e3ac2762d5f/LICENSE-APACHE-2.0.txt) |
| `github.com/xeipuuv/gojsonreference` | bd5ef7bd5415 | Apache-2.0 | [license](https://github.com/xeipuuv/gojsonreference/blob/bd5ef7bd5415/LICENSE-APACHE-2.0.txt) |
| `github.com/xeipuuv/gojsonschema` | v1.2.0 | Apache-2.0 | [license](https://github.com/xeipuuv/gojsonschema/blob/v1.2.0/LICENSE-APACHE-2.0.txt) |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | [license](https://github.com/yosida95/uritemplate/blob/v3.0.2/LICENSE) |
| `golang.org/x/oauth2` | v0.36.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/oauth2/+/v0.36.0:LICENSE) |
| `golang.org/x/sync` | v0.20.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/sync/+/v0.20.0:LICENSE) |
| `golang.org/x/sys/cpu` | v0.41.0 | BSD-3-Clause | [license](https://cs.opensource.google/go/x/sys/+/v0.41.0:LICENSE) |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT | [license](https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE) |

## Notes

- **`stretchr/testify`, `davecgh/go-spew`, and `pmezard/go-difflib` are here on
  purpose. Do not remove them as "test-only".** They are in the binary's
  dependency closure because `github.com/maypok86/otter/v2` v2.3.0 imports
  `testify/require` from a file named `issue_test_1.25.go`. That name does not end
  in `_test.go`, so Go compiles it as ordinary package code rather than treating it
  as a test, and testify plus its two dependencies come with it. Confirmed from
  both `go-licenses` and `go list -deps ./cmd/server`. All three are permissive
  (MIT, ISC, BSD-3-Clause), so this is an accuracy matter, not a licence problem.
  If a later Otter release renames that file, these three drop out and
  `.github/scripts/licenses-check.sh` will say so.
- `github.com/go-jose/go-jose/v4` bundles a `json` subpackage under BSD-3-Clause
  (a copy of the Go standard library's encoding/json); both are permissive.
- `github.com/modelcontextprotocol/go-sdk` is mid-transition from MIT to
  Apache 2.0; un-relicensed files may remain MIT. Both are permissive.
- `gopkg.in/yaml.v3` ships a NOTICE (Apache 2.0) alongside MIT-licensed
  libyaml-derived files; its required attribution is reproduced in `NOTICE`.
- `github.com/oasdiff/yaml3` is a fork of `gopkg.in/yaml.v3` and carries the same
  dual MIT / Apache-2.0 split and the same Canonical NOTICE. The generator
  resolves it to MIT, which is what the table records; `NOTICE` now names it
  alongside `gopkg.in/yaml.v3` so the Apache-2.0 attribution is propagated for
  both. It was previously missing.
- Commit-pinned modules (`mohae/deepcopy`, `oasdiff/*`, `xeipuuv/*`) carry a
  clear license at the pinned commit.
