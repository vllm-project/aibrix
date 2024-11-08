from typing import Optional, Iterable, Tuple
from functools import reduce
import numpy as np
import logging

from .types import GPUProfile, WorkloadProfile
from .solver.melange import Config as MelangConfig, SolverRunner

logger = logging.getLogger("aibrix.gpuoptimizer.optimizer")

class Optimizer:
    def __init__(self, profiles: Optional[Iterable[GPUProfile]] = None):
        self.config = MelangConfig()
        self.workload_distribution_template = None
        if profiles != None:
            for profile in profiles:
                self.add_profile(profile)

    def set_profile(self, profile: GPUProfile):
        if self.workload_distribution_template == None:
            self.workload_distribution_template = np.zeros_like(profile.tputs)
        elif self.workload_distribution_template.shape != np.shape(profile.tputs):
            raise Exception("Profile added should keep a same shape")
        
        self.config.gpu_info[profile.gpu] = profile.__dict__

    def delete_profile(self, gpu):
        if gpu in self.config.gpu_info:
            del self.config.gpu_info[gpu]

    def set_workload_distribution(self, profiles: Iterable[WorkloadProfile]) -> bool:
        """Update workload distribution and return success or failure."""
        self.config.total_request_rate = reduce(lambda cnt, center: cnt + center.rate, profiles, 0)
        success = True
        for profile in profiles:
            try:
                self.workload_distribution_template[self._validate_workload_index_signature(profile.signature)] = profile.rate / self.config.total_request_rate
            except Exception as e:
                logger.error(f"Fail to set workload distribution: {profile.signature}: {e}")
                success = False
        self.config.workload_distribution = self.workload_distribution_template.tolist()
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
        logger.debug(f"Starting solver for {self.config.gpu_info.keys()}")
        if len(self.config.gpu_info) == 0:
            return None
        
        runner = SolverRunner(self.config)
        ret = runner.run()
        logger.debug(f"Done solver: {ret}")
        return ret

    def _validate_workload_index_signature(self, signature: Tuple) -> Tuple:
        """Validate workload's signature by regard each element in signature tuple a index.
        return valid index tuple for accessing  self.workload_distribution_template"""
        if len(signature) != self.workload_distribution_template.ndim:
            raise Exception(f"Unmatch workload profile, expected a signature of length {self.workload_distribution_template.ndim} , got {len(signature)}.")
        
        # No validation on the shape. Leave set function to throw error
        return signature
    

            
