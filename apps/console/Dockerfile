# AIBrix Console
#
# Multi-stage build that produces a single image containing the Go HTTP+gRPC
# backend and the React SPA. The Go server serves the SPA from STATIC_FILES_DIR
# and falls back to index.html for client-side routes.
#
# The React SPA is built on the host (Docker doesn't need a Node image) and
# COPYed in via apps/console/web/build/. Build context must be the repo root.
#
# Commands:
#   (cd apps/console/web && npm install && npm run build)
#   docker build -f apps/console/Dockerfile -t aibrix/console .

# Stage 1: build the Go binary.
# golang:1.22 (debian) is intentional — gorm.io/driver/sqlite needs CGO and
# the alpine variant requires extra build-base apk packages.
FROM golang:1.22 AS api
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# nozmq: console doesn't need ZeroMQ (gateway-plugins-only feature).
# CGO_ENABLED=1: required by the SQLite driver used for the dev/file store.
RUN CGO_ENABLED=1 go build -tags nozmq -o /out/console ./cmd/console

# Stage 2: minimal runtime.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=api /out/console /app/console
COPY apps/console/web/build /app/web
ENV STATIC_FILES_DIR=/app/web \
    HTTP_ADDR=:8080 \
    GRPC_ADDR=:50060
EXPOSE 8080 50060
ENTRYPOINT ["/app/console"]
