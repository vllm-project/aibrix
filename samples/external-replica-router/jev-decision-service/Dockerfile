# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0.

FROM golang:1.22.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY samples/external-replica-router/jev-decision-service/ samples/external-replica-router/jev-decision-service/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/jev-decision-service ./samples/external-replica-router/jev-decision-service

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/jev-decision-service /jev-decision-service
EXPOSE 8080
ENTRYPOINT ["/jev-decision-service"]
