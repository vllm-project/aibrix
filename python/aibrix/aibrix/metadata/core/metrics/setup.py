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

from __future__ import annotations

from dataclasses import dataclass

from prometheus_client import CollectorRegistry, make_asgi_app
from starlette.types import ASGIApp

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.prometheus import PrometheusSink
from aibrix.metadata.core.metrics.sink import Emitter, FanoutSink, NoopSink, Sink
from aibrix.metadata.core.metrics.udp import DogStatsdSink, StatsdSink, StatsiteSink
from aibrix.metadata.setting import (
    MetricsConfig,
    load_metrics_config,
)

logger = init_logger(__name__)


@dataclass
class MetricsRuntime:
    sink: Sink
    registry: CollectorRegistry | None = None
    metrics_app: ASGIApp | None = None


def setup_metrics(config: MetricsConfig | None = None) -> MetricsRuntime:
    config = config if config is not None else load_metrics_config()
    if config is None:
        sink: Sink = NoopSink()
        Emitter.set_sink(sink)
        return MetricsRuntime(sink=sink)

    sinks: list[Sink] = []
    registry: CollectorRegistry | None = None
    metrics_app = None

    if config.prometheus_enabled:
        registry = CollectorRegistry()
        sinks.append(PrometheusSink(registry))
        metrics_app = make_asgi_app(registry=registry)

    if config.statsite_addr:
        sinks.append(StatsiteSink(config.statsite_addr, prefix=config.service_name))

    if config.statsd_addr:
        sinks.append(StatsdSink(config.statsd_addr, prefix=config.service_name))

    if config.dogstatsd_addr:
        sinks.append(
            DogStatsdSink(
                config.dogstatsd_addr,
                prefix=config.service_name,
                global_tags=config.dogstatsd_tags,
            )
        )

    if not sinks:
        logger.warning("Metrics configured without any enabled sink; using noop sink")
        sink = NoopSink()
        Emitter.set_sink(sink)
        return MetricsRuntime(sink=sink, registry=registry, metrics_app=metrics_app)

    sink = sinks[0] if len(sinks) == 1 else FanoutSink(sinks)
    Emitter.set_sink(sink)
    return MetricsRuntime(sink=sink, registry=registry, metrics_app=metrics_app)


def shutdown_metrics() -> None:
    Emitter.close()
    Emitter.reset()
