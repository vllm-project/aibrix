# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from prometheus_client import CollectorRegistry, Counter, Histogram, Info

REGISTRY = CollectorRegistry()

INFO_METRICS = Info(
    name="aibrix:info",
    documentation="AIBrix Info",
    # labelnames=["version", "engine", "engine_version"],
    registry=REGISTRY,
)

HTTP_COUNTER_METRICS = Counter(
    name="aibrix:api_request_total",
    documentation="Count of AIBrix API Requests by method, endpoint and status",
    labelnames=["method", "endpoint", "status"],
    registry=REGISTRY,
)
HTTP_LATENCY_METRICS = Histogram(
    name="aibrix:api_request_latency",
    documentation="Latency of AIBrix API Requests by method, endpoint and status",
    labelnames=["method", "endpoint", "status"],
    buckets=[0.1, 0.2, 0.5, 1, 2, 5],
    registry=REGISTRY,
)
