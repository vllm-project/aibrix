# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

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
