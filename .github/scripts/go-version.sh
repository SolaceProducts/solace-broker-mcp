#!/usr/bin/env bash
#
# Print go.mod's `go` directive (e.g. 1.26.10), the Go version the Dockerfile's
# builder stage runs. Every image build passes it as the GO_VERSION build arg:
# the Makefile `docker` target, build-image.yaml, dev-build.yaml and release.yml.
# The release binaries get the same version from setup-go's go-version-file, so
# reading it here, once, keeps the image and the binaries on one Go.
#
# Usage: .github/scripts/go-version.sh   (from the repository root)

set -euo pipefail

awk '$1 == "go" { print $2; exit }' go.mod
