# Builder stage only - for building gateway-plugins binary
ARG BUILDER_IMAGE=golang:1.22
ARG TARGETPLATFORM

FROM ${BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

RUN apt-get update && apt-get install -y \
    libkrb5-dev libzmq3-dev

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    pkg-config \
    libkrb5-dev \
    libzmq3-dev \
    libsodium-dev \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY pkg/ pkg/

ENV CGO_ENABLED=1
ENV CGO_LDFLAGS="-lzmq -lsodium -lpthread -lm -lstdc++"

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -tags=zmq \
    -ldflags="-s -w -linkmode=external -extldflags '${CGO_LDFLAGS}'" \
    -o gateway-plugins ./cmd/plugins

# Gather shared libs
RUN set -eux; \
    mkdir -p /workspace/deps; \
    ldd /workspace/gateway-plugins \
      | tr -s '[:blank:]' '\n' \
      | grep '^/' \
      | sort -u \
      | xargs -r -I{} cp -v {} /workspace/deps/
