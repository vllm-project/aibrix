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


from abc import abstractmethod
from typing import Iterable

from prometheus_client import Metric
from prometheus_client.samples import Sample


class StandardRule:
    def __init__(self, rule_type):
        self.rule_type = rule_type

    @abstractmethod
    def __call__(self, metric: Metric) -> Iterable[Metric]:
        pass


class RenameStandardRule(StandardRule):
    def __init__(self, original_name, new_name):
        super().__init__("RENAME")
        self.original_name = original_name
        self.new_name = new_name

    def __call__(self, metric: Metric) -> Iterable[Metric]:
        assert (
            metric.name == self.original_name
        ), f"Metric name {metric.name} does not match Rule original name {self.original_name}"
        metric.name = self.new_name

        # rename all the samples
        _samples = []
        for s in metric.samples:
            s_name = self.new_name + s.name[len(self.original_name) :]
            _samples.append(
                Sample(
                    s_name,
                    s.labels,
                    s.value,
                    s.timestamp,
                    s.exemplar,
                )
            )
        metric.samples = _samples
        yield metric
