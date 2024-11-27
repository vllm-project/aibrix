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
from dataclasses import dataclass, field
from typing import Callable, List, Optional, Protocol, Tuple


@dataclass
class GPUProfile:
    """Support json input like:
    {
        "gpu": "A10",
        "cost": 1.01,
        "tputs": [[3, 2, 1], [5, 2, 1]],
        "indexes: [[512, 1024], [32, 64, 128]]
    }
    where tputs is formulated as:

    | RPS | # OUTs 1 | # OUTs 2 |
    |---|---|---|
    | # INs 1 | 2 | 1 |
    | # INs s 2 | 5 | 2 |
    """

    gpu: str = ""
    cost: float = 0.0
    tputs: list = field(default_factory=list)  # units: requests per second
    indexes: list = field(default_factory=list)  # value ticks of tputs columns and rows
    created: float = 0.0


WorkloadSignatureErrorHandler = Callable[[int, float, float, float, float], None]
"""A function to handle the error with parameters(dimension, value, index assigned, value of index, value offset)."""


class WorkloadProfile(Protocol):
    """Description of worklaod characteristic"""

    def get_signature(
        self,
        indexes: List[List[float]],
        error_suppressor: Optional[WorkloadSignatureErrorHandler] = None,
    ) -> Tuple[int]:
        """Generate the index signature of the WorkloadProfile within the indexes' range.

        Args:
            indexes: A list of list of float, each list is a range of values.
            error_suppressor: A callback to suppress possible error. If None, raise an exception.
        """

    @property
    def signature(self) -> Tuple[int]:
        """The signature of the workload"""

    @property
    def rate(self) -> float:
        """The request rate of the workload in the unit RPS"""
