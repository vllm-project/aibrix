import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Union

from .solver import MelangeSolver, Solver

PROJECT_DIR = Path(__file__).parent.parent.parent


@dataclass
class Config:
    gpu_info: dict = field(default_factory=dict)
    workload_distribution: list = field(default_factory=list)
    total_request_rate: float = 0  # units: requests per second
    slice_factor: int = 4


# This class is adapted from code originally written by Tyler Griggs
# found at https://github.com/tyler-griggs/melange-release
# See: https://tyler-griggs.github.io/blogs/melange
class SolverRunner:
    def __init__(self, config: Union[str, Config]):
        if isinstance(config, str):
            config = Config(**json.load(open(config)))
        self.config: Config = config
        self.solver: Solver = MelangeSolver(
            workload_distribution=self.config.workload_distribution,
            total_request_rate=self.config.total_request_rate,
            gpu_info=self.config.gpu_info,
            slice_factor=self.config.slice_factor,
        )
        self.execution_result = {}  # type: ignore

    def run(self):
        self.execution_result = self.solver.run()
        return self.execution_result

    def export(self, path):
        with open(path, "w") as f:
            json.dump(self.execution_result, f, indent=4)
