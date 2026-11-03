FROM golang:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY test/e2e/external-metrics-adapter/ test/e2e/external-metrics-adapter/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o external-metrics-adapter ./test/e2e/external-metrics-adapter

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/external-metrics-adapter .
USER 65532:65532

ENTRYPOINT ["/external-metrics-adapter"]
