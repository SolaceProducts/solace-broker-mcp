#!/usr/bin/env bash
#
# Print go.mod's `go` directive (e.g. 1.26.10), the Go version the Dockerfile's
# builder stage runs. Every image build passes it as the GO_VERSION build arg:
# the Makefile `docker` target, build-image.yaml, dev-build.yaml and release.yml.
# The release binaries get the same version from setup-go's go-version-file, so
# reading it here, once, keeps the image and the binaries on one Go.
#
# Refuses, rather than prints, two go.mod shapes that would split them again:
#
#   - A `go` line that is not a full X.Y.Z. `go 1.26` is valid Go, but it would
#     select the floating `golang:1.26-alpine`, and setup-go resolves a bare
#     `1.26` to the newest patch on its own schedule. A CRLF go.mod or a
#     missing `go` line lands here too, with a clearer error than the FROM line
#     would give.
#   - A `toolchain` line. setup-go prefers it over `go`, while the golang image
#     (GOTOOLCHAIN=local) builds with GO_VERSION. The Dockerfile refuses it as
#     well, for a build that passes GO_VERSION by hand.
#
# The build-and-test lint job runs this on every pull request, forks included,
# which never build the image.
#
# Usage: .github/scripts/go-version.sh [path/to/go.mod]   (default: ./go.mod)

set -euo pipefail

gomod="${1:-go.mod}"

if grep -qE '^toolchain[[:space:]]' "$gomod"; then
    echo "$gomod has a toolchain line. setup-go would build the release binaries with it, but the image builds with the go line, so the two would ship different Go versions. Raise the go line instead and remove the toolchain line." >&2
    exit 1
fi

version=$(awk '$1 == "go" { print $2; exit }' "$gomod")
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "$gomod: the go line must be a full X.Y.Z version, got $(printf '%q' "$version"). A two-part version such as 1.26 would select a floating golang image tag. Check also for a missing go line or CRLF line endings." >&2
    exit 1
fi

echo "$version"
