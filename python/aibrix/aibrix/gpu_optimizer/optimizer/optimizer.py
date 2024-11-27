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
import logging
from functools import reduce
from typing import Iterable, Optional, Tuple

import numpy as np

from .solver.melange import Config as MelangConfig
from .solver.melange import SolverRunner
from .types import GPUProfile, WorkloadProfile

logger = logging.getLogger("aibrix.gpuoptimizer.optimizer")


class Optimizer:
    def __init__(self, profiles: Optional[Iterable[GPUProfile]] = None):
        self._config = MelangConfig()
        self._workload_distribution_template: Optional[np.ndarray] = None
        self._indexes: Optional[list] = None  # Values ticks of tputs columns and rows
        if profiles is not None:
            for profile in profiles:
                self.set_profile(profile)

    def set_profile(self, profile: GPUProfile):
        if self._workload_distribution_template is None:
            self._workload_distribution_template = np.zeros_like(profile.tputs)
            self._indexes = profile.indexes
        elif (
            self._workload_distribution_template.shape != np.shape(profile.tputs)
            or self._indexes != profile.indexes
        ):
            raise Exception(
                f"Profile({profile.gpu}) applied should keep a same shape and value ticks. shapes: {self._workload_distribution_template.shape} vs {np.shape(profile.tputs)}, indexes: {self._indexes} vs {profile.indexes}"
            )

        logger.debug(
            "Applied profile for %s, shape: %s, indees: %s",
            profile.gpu,
            profile.tputs,
            self._indexes,
        )
        self._config.gpu_info[profile.gpu] = profile.__dict__

    def delete_profile(self, gpu):
        if gpu in self._config.gpu_info:
            del self._config.gpu_info[gpu]

    def set_workload_distribution(
        self, profiles: Iterable[WorkloadProfile], total_request_rate: int
    ) -> bool:
        """Update workload distribution and return success or failure."""
        if self._workload_distribution_template is None:
            return False

        # Maintain the overall request scale disregard some request are not covered.
        self._config.total_request_rate = total_request_rate
        # covered_request_rate is used to calculate the workload distribution.
        covered_request_rate = reduce(
            lambda cnt, center: cnt + center.rate, profiles, 0.0
        )
        success = True
        for profile in profiles:
            try:
                self._workload_distribution_template[
                    self._validate_workload_signature(profile)
                ] = profile.rate / covered_request_rate  # type: ignore
            except Exception as e:
                logger.error(
                    f"Fail to set workload distribution: {profile.signature}: {e}"
                )
                success = False
        self._config.workload_distribution = (
            self._workload_distribution_template.tolist()
        )
        return success

    def run(self) -> Optional[dict]:
        """Run the solver and return the result.
        Return None if no profiles are added.
        The result is a dict with the following format:

        {
            "gpu1": replicas1,
            "gpu1": replicas2,
            "cost": cost,
        }
        """
        logger.debug(f"Starting solver for {self._config.gpu_info.keys()}")
        if len(self._config.gpu_info) == 0:
            return None

        runner = SolverRunner(self._config)
        ret = runner.run()
        logger.debug(f"Done solver: {ret}")
        return ret

    def _validate_workload_signature(self, profile: WorkloadProfile) -> Tuple[int]:
        """Validate workload's signature by regard each element in signature tuple a index.
        return valid index tuple for accessing  self._workload_distribution_template"""
        if self._workload_distribution_template is None or self._indexes is None:
            raise Exception("Load profile not set.")

        signature = profile.get_signature(self._indexes, self._log_signature_error)
        if len(signature) != self._workload_distribution_template.ndim:
            raise Exception(
                f"Unmatch workload profile, expected a signature of length {self._workload_distribution_template.ndim} , got {len(signature)}."
            )

        # No validation on the shape. Leave set function to throw error
        return signature

    def _log_signature_error(self, dimeansion, value, index, index_value, offset):
        logger.warning(
            f"Signature item {dimeansion}:{value} is out of range, counted as{index_value} (reference offset: {offset})"
        )
