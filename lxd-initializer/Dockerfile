# Build the manager binary
ARG BUILDER_GOLANG_VERSION=1.26.7
# First stage: build the executable.
FROM us-central1-docker.pkg.dev/palette-images-dev/hardened-images/builder/golang:${BUILDER_GOLANG_VERSION}-alpine AS toolchain

FROM toolchain AS builder
WORKDIR /workspace

RUN apk update
RUN apk add git gcc g++ curl

# Install build dependencies
RUN apk add --no-cache git make

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies with cache
RUN --mount=type=cache,target=/root/.local/share/golang \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY *.go ./

# Build the binary (respect multi-arch via Docker BuildKit target arch)
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o lxd-initializer .

# Create final image
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache lxc curl jq

# Copy the binary from the builder stage
COPY --from=builder /workspace/lxd-initializer /lxd-initializer

# Set the entrypoint
ENTRYPOINT ["/lxd-initializer"]
