# Stage 1: Build
FROM golang:1.25-alpine AS builder

ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=${VERSION}" \
    -o /solace-broker-mcp \
    ./cmd/server

# Stage 2: Runtime
FROM gcr.io/distroless/static-debian12:nonroot

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
