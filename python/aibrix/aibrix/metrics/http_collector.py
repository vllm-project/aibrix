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

from copy import deepcopy
from typing import Dict, Optional

import requests
from prometheus_client.parser import text_string_to_metric_families
from prometheus_client.registry import Collector

from aibrix import envs
from aibrix.config import DEFAULT_METRIC_COLLECTOR_TIMEOUT
from aibrix.logger import init_logger
from aibrix.metrics.standard_rules import StandardRule

logger = init_logger(__name__)


class HTTPCollector(Collector):
    def __init__(
        self,
        endpoint: str,
        metrics_rules: Optional[Dict[str, StandardRule]] = None,
        keep_original_metric: bool = True,
        timeout=DEFAULT_METRIC_COLLECTOR_TIMEOUT,
        raw_passthrough_mode: Optional[bool] = None,
    ):
        self.metric_endpoint = endpoint
        self.metrics_rules = metrics_rules or {}
        self.keep_original_metric = keep_original_metric

        # Determine mode: raw passthrough overrides transformation
        if raw_passthrough_mode is not None:
            self.raw_passthrough_mode = raw_passthrough_mode
        else:
            self.raw_passthrough_mode = envs.METRICS_RAW_PASSTHROUGH_MODE

        # If transformation is disabled, force raw passthrough
        if not envs.METRICS_ENABLE_TRANSFORMATION:
            self.raw_passthrough_mode = True

        self.timeout = timeout
        self.session = requests.Session()

    def _collect(self):
        try:
            response = self.session.get(self.metric_endpoint, timeout=self.timeout)
            if response.status_code != 200:
                logger.warning(
                    f"Failed to collect metrics from {self.metric_endpoint} "
                    f"with status code {response.status_code}, "
                    f"response: {response.text}"
                )
                return ""
            return response.text
        except Exception as e:
            logger.warning(
                f"Failed to collect metrics from {self.metric_endpoint}: {e}"
            )
            return ""

    def collect(self):
        metrics_text = self._collect()

        # Raw passthrough mode: return all metrics unchanged
        if self.raw_passthrough_mode:
            logger.debug("Raw passthrough mode enabled - returning metrics unchanged")
            for m in text_string_to_metric_families(metrics_text):
                yield m
            return

        # Standard transformation mode
        try:
            for m in text_string_to_metric_families(metrics_text):
                if m.name in self.metrics_rules:
                    if self.keep_original_metric:
                        yield deepcopy(m)
                    new_metric = self.metrics_rules[m.name](m)
                    if new_metric is not None:
                        yield from new_metric
                elif self.keep_original_metric:
                    yield m
        except Exception as e:
            logger.error(f"Error during metrics transformation: {e}")
            # Fallback to raw passthrough on transformation errors
            logger.warning(
                "Falling back to raw passthrough mode due to transformation error"
            )
            for m in text_string_to_metric_families(metrics_text):
                yield m
