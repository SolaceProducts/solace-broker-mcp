# The builder's Go version is go.mod's `go` directive, passed in by every build
# (Makefile `docker`, build-image.yaml, dev-build.yaml, release.yml), so the image
# and the release tarballs are built by the same toolchain. No default on
# purpose: a floating `golang:1.26-alpine` is what let them diverge. The error
# text cannot contain spaces, which would split the FROM line.
ARG GO_VERSION

# Stage 1: Build
FROM golang:${GO_VERSION:?pass-build-arg-GO_VERSION-set-to-the-go-directive-in-go.mod}-alpine AS builder

ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
# A toolchain line would split the image from the release binaries: setup-go
# builds those with it, while this image (GOTOOLCHAIN=local) builds with
# GO_VERSION from the go line. Raise the go line instead.
RUN if grep -qE '^toolchain[[:space:]]' go.mod; then \
        echo "go.mod has a toolchain line; the image and the release binaries would ship different Go versions. Raise the go line instead and remove it." >&2; \
        exit 1; \
    fi
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=${VERSION}" \
    -o /solace-broker-mcp \
    ./cmd/server

# Stage 2: Runtime
# Pinned by digest so a rebuild of a tag ships the same runtime base;
# Dependabot's docker entry proposes each new digest of the tag.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source=https://github.com/SolaceProducts/solace-broker-mcp

COPY --from=builder /solace-broker-mcp /solace-broker-mcp

# Apache-2.0 section 4(d): a derivative work we distribute has to carry a readable
# copy of the attribution notices from our dependencies' NOTICE files. This image
# is a distribution of that derivative work, so the notices ship in it. Without
# these three the image satisfied section 4(d) for nothing it links.
COPY --from=builder /src/LICENSE /src/NOTICE /src/THIRD_PARTY_LICENSES.md /licenses/

EXPOSE 9090

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/solace-broker-mcp", "--health"]

ENTRYPOINT ["/solace-broker-mcp"]
