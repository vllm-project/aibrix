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
        self._workload_distribution_template = None
        self._indexes = None # Values ticks of tputs columns and rows
        if profiles != None:
            for profile in profiles:
                self.add_profile(profile)

    def set_profile(self, profile: GPUProfile):
        if self._workload_distribution_template is None:
            self._workload_distribution_template = np.zeros_like(profile.tputs)
            self._indexes = profile.indexes
        elif (self._workload_distribution_template.shape != np.shape(profile.tputs) or
            self._indexes != profile.indexes):
            raise Exception(f"Profile({profile.gpu}) applied should keep a same shape and value ticks. shapes: {self._workload_distribution_template.shape} vs {np.shape(profile.tputs)}, indexes: {self._indexes} vs {profile.indexes}")
        
        logger.debug("Applied profile for %s, shape: %s, indees: %s", profile.gpu, profile.tputs, self._indexes)
        self._config.gpu_info[profile.gpu] = profile.__dict__

    def delete_profile(self, gpu):
        if gpu in self._config.gpu_info:
            del self._config.gpu_info[gpu]

    def set_workload_distribution(self, profiles: Iterable[WorkloadProfile], total_request_rate: int) -> bool:
        """Update workload distribution and return success or failure."""
        # Maintain the overall request scale disregard some request are not covered.
        self._config.total_request_rate = total_request_rate 
        # covered_request_rate is used to calculate the workload distribution.
        covered_request_rate = reduce(lambda cnt, center: cnt + center.rate, profiles, 0)
        success = True
        for profile in profiles:
            try:
                self._workload_distribution_template[self._validate_workload_signature(profile)] = profile.rate / covered_request_rate
            except Exception as e:
                logger.error(f"Fail to set workload distribution: {profile.signature}: {e}")
                success = False
        self._config.workload_distribution = self._workload_distribution_template.tolist()
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
        signature = profile.get_signature(self._indexes, self._log_signature_error)
        if len(signature) != self._workload_distribution_template.ndim:
            raise Exception(f"Unmatch workload profile, expected a signature of length {self._workload_distribution_template.ndim} , got {len(signature)}.")
        
        # No validation on the shape. Leave set function to throw error
        return signature
    
    def _log_signature_error(self, dimeansion, value, index, index_value, offset):
        logger.warning(f"Signature item {dimeansion}:{value} is out of range, counted as{index_value} (reference offset: {offset})")
    

            
