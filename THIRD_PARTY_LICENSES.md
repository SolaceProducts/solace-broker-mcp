# Third-Party Licenses

This file lists the third-party components compiled into the `solace-broker-mcp`
binary, with their versions and licenses. It is the human-readable OSS compliance
inventory that accompanies the release.

**Generated** 2026-07-17 with
[`go-licenses`](https://github.com/google/go-licenses) against
`./cmd/server`. Regenerate before each release. The FOSSA scan in CI is the
authoritative automated check; this file complements it.

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
| `github.com/dolthub/maphash` | v0.1.0 | Apache-2.0 | [license](https://github.com/dolthub/maphash/blob/v0.1.0/LICENSE) |
| `github.com/gammazero/deque` | v0.2.1 | MIT | [license](https://github.com/gammazero/deque/blob/v0.2.1/LICENSE) |
| `github.com/getkin/kin-openapi` | v0.134.0 | MIT | [license](https://github.com/getkin/kin-openapi/blob/v0.134.0/LICENSE) |
| `github.com/go-jose/go-jose/v4` | v4.1.4 | Apache-2.0 | [license](https://github.com/go-jose/go-jose/blob/v4.1.4/LICENSE) |
| `github.com/go-jose/go-jose/v4/json` | v4.1.4 | BSD-3-Clause | [license](https://github.com/go-jose/go-jose/blob/v4.1.4/json/LICENSE) |
| `github.com/go-openapi/jsonpointer` | v0.21.0 | Apache-2.0 | [license](https://github.com/go-openapi/jsonpointer/blob/v0.21.0/LICENSE) |
| `github.com/go-openapi/swag` | v0.23.0 | Apache-2.0 | [license](https://github.com/go-openapi/swag/blob/v0.23.0/LICENSE) |
| `github.com/google/jsonschema-go/jsonschema` | v0.4.2 | MIT | [license](https://github.com/google/jsonschema-go/blob/v0.4.2/LICENSE) |
| `github.com/josharian/intern` | v1.0.0 | MIT | [license](https://github.com/josharian/intern/blob/v1.0.0/license.md) |
| `github.com/mailru/easyjson` | v0.7.7 | MIT | [license](https://github.com/mailru/easyjson/blob/v0.7.7/LICENSE) |
| `github.com/maypok86/otter` | v1.2.4 | Apache-2.0 | [license](https://github.com/maypok86/otter/blob/v1.2.4/LICENSE) |
| `github.com/modelcontextprotocol/go-sdk` | v1.5.0 | Apache-2.0 | [license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.5.0/LICENSE) |
| `github.com/mohae/deepcopy` | c48cc78d4826 | MIT | [license](https://github.com/mohae/deepcopy/blob/c48cc78d4826/LICENSE) |
| `github.com/oasdiff/yaml` | a3ea61cb4d4c | MIT | [license](https://github.com/oasdiff/yaml/blob/a3ea61cb4d4c/LICENSE) |
| `github.com/oasdiff/yaml3` | 61cd415a242b | MIT | [license](https://github.com/oasdiff/yaml3/blob/61cd415a242b/LICENSE) |
| `github.com/perimeterx/marshmallow` | v1.1.5 | MIT | [license](https://github.com/perimeterx/marshmallow/blob/v1.1.5/LICENSE) |
| `github.com/segmentio/asm` | v1.1.3 | MIT | [license](https://github.com/segmentio/asm/blob/v1.1.3/LICENSE) |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | [license](https://github.com/segmentio/encoding/blob/v0.5.4/LICENSE) |
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

- `github.com/go-jose/go-jose/v4` bundles a `json` subpackage under BSD-3-Clause
  (a copy of the Go standard library's encoding/json); both are permissive.
- `github.com/modelcontextprotocol/go-sdk` is mid-transition from MIT to
  Apache 2.0; un-relicensed files may remain MIT. Both are permissive.
- `gopkg.in/yaml.v3` ships a NOTICE (Apache 2.0) alongside MIT-licensed
  libyaml-derived files; its required attribution is reproduced in `NOTICE`.
- Commit-pinned modules (`mohae/deepcopy`, `oasdiff/*`, `xeipuuv/*`) carry a
  clear license at the pinned commit.
