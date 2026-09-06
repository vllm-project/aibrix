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

from dataclasses import dataclass, field
from typing import Optional

from aibrix import envs


@dataclass(frozen=True)
class MetricsConfig:
    service_name: str = "aibrix-metadata"
    prometheus_enabled: bool = False
    statsd_addr: str = ""
    statsite_addr: str = ""
    dogstatsd_addr: str = ""
    dogstatsd_tags: list[str] = field(default_factory=list)


def load_public_metrics_config() -> Optional[MetricsConfig]:
    config = MetricsConfig(
        service_name=envs.METRICS_SERVICE_NAME,
        prometheus_enabled=envs.METRICS_PROMETHEUS_ENABLED,
        statsd_addr=envs.METRICS_STATSD_ADDR,
        statsite_addr=envs.METRICS_STATSITE_ADDR,
        dogstatsd_addr=envs.METRICS_DOGSTATSD_ADDR,
        dogstatsd_tags=list(envs.METRICS_DOGSTATSD_TAGS),
    )

    if (
        not config.prometheus_enabled
        and not config.statsd_addr
        and not config.statsite_addr
        and not config.dogstatsd_addr
    ):
        return None

    return config


def load_metrics_config() -> Optional[MetricsConfig]:
    return load_public_metrics_config()
