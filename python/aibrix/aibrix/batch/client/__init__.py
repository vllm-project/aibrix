# Copyright 2026 The Aibrix Team.
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
"""Reusable inference client + dispatch engine.

A standalone capability with zero ``aibrix.batch`` dependency, consumed by the
batch job drivers and (later) by benchmarks. Layering:

  Channel        transport over one endpoint (http / echo)
  EndpointSource where channels come from + capacity (topology seam)
  DispatchEngine policy-driven concurrent loop (ordering / routing / pacing)

Callers translate ``InferenceError`` into their own domain error at the boundary.
"""

from __future__ import annotations

from aibrix.batch.client.channel import (
    Channel,
    EchoChannel,
    HttpChannel,
    InferenceRequest,
    Response,
)
from aibrix.batch.client.concurrency import (
    ConcurrencyController,
    ConcurrencyOutcome,
    FixedConcurrencyController,
    LLMAdaptiveConcurrencyController,
    LLMAdaptiveConcurrencySettings,
    concurrency_outcome_from_result,
)
from aibrix.batch.client.engine import (
    DispatchEngine,
    DispatchStats,
    DispatchStatsSnapshot,
    OnResult,
    RetryConfig,
)
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.policy import FIFO, RoundRobin, Router, Scheduler
from aibrix.batch.client.source import CapacitySignal, EndpointSource
from aibrix.batch.client.sources import (
    DiscoveryEndpointSource,
    GatewayEndpointSource,
    InClusterEndpointSource,
    NoopEndpointSource,
    PortForwardEndpointSource,
    StaticEndpointSource,
)

__all__ = [
    # transport
    "Channel",
    "HttpChannel",
    "EchoChannel",
    "InferenceRequest",
    "Response",
    # concurrency
    "ConcurrencyController",
    "ConcurrencyOutcome",
    "FixedConcurrencyController",
    "LLMAdaptiveConcurrencyController",
    "LLMAdaptiveConcurrencySettings",
    "concurrency_outcome_from_result",
    # source
    "EndpointSource",
    "CapacitySignal",
    "StaticEndpointSource",
    "GatewayEndpointSource",
    "NoopEndpointSource",
    "InClusterEndpointSource",
    "PortForwardEndpointSource",
    "DiscoveryEndpointSource",
    # engine + policy
    "DispatchEngine",
    "DispatchStats",
    "DispatchStatsSnapshot",
    "OnResult",
    "RetryConfig",
    "Router",
    "Scheduler",
    "RoundRobin",
    "FIFO",
    # errors
    "InferenceError",
    "InferenceErrorCode",
]
