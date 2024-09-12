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

from typing import Dict

import requests
from prometheus_client.parser import text_string_to_metric_families
from prometheus_client.registry import Collector

from aibrix.config import DEFAULT_METRIC_COLLECTOR_TIMEOUT
from aibrix.logger import init_logger
from aibrix.metrics.standard_rules import StandardRule

logger = init_logger(__name__)


class HTTPCollector(Collector):
    def __init__(
        self,
        endpoint: str,
        metrics_rules: Dict[str, StandardRule],
        keep_original_metric: bool = True,
        timeout=DEFAULT_METRIC_COLLECTOR_TIMEOUT,
    ):
        self.metric_endpoint = endpoint
        self.metrics_rules = metrics_rules
        self.keep_original_metric = keep_original_metric

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
        for m in text_string_to_metric_families(metrics_text):
            if self.keep_original_metric:
                yield m

            # metric standardizing rule matched
            if m.name in self.metrics_rules:
                new_metric = self.metrics_rules[m.name](m)
                if new_metric is not None:
                    yield from new_metric
