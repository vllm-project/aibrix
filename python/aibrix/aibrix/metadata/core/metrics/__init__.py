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

from aibrix.metadata.setting import MetricsConfig, load_metrics_config

from . import names as metrics_names
from .setup import MetricsRuntime, setup_metrics, shutdown_metrics
from .sink import Emitter, FanoutSink, NoopSink, T, Tag, duration_ms
from .tracking import (
    begin_backend_operation_count,
    get_backend_operation_count,
    record_backend_operation,
    reset_backend_operation_count,
)

__all__ = [
    "Emitter",
    "FanoutSink",
    "MetricsConfig",
    "MetricsRuntime",
    "NoopSink",
    "T",
    "Tag",
    "begin_backend_operation_count",
    "duration_ms",
    "get_backend_operation_count",
    "load_metrics_config",
    "metrics_names",
    "record_backend_operation",
    "reset_backend_operation_count",
    "setup_metrics",
    "shutdown_metrics",
]
