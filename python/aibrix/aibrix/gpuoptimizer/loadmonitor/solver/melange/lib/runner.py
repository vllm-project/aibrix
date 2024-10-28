from dataclasses import dataclass, field
import json
from typing import Dict
from pathlib import Path

from melange.solver import MelangeSolver, Solver

PROJECT_DIR = Path(__file__).parent.parent.parent

class SolverRunner:
    @dataclass
    class Config:
        gpu_info: dict = field(default_factory=dict)
        workload_distribution: list = field(default_factory=list)
        total_request_rate: float = 0 # units: requests per second
        slice_factor: int = 1

    def __init__(self, config_path: str):
        self.config: SolverRunner.Config = SolverRunner.Config(**json.load(open(config_path)))
        self.solver: Solver = MelangeSolver(
            workload_distribution=self.config.workload_distribution,
            total_request_rate=self.config.total_request_rate,
            gpu_info=self.config.gpu_info,
            slice_factor=self.config.slice_factor
        )
        self.execution_result = {}

    def run(self):
        self.execution_result = self.solver.run()
        print(f"[Melange] Recommendation: {self.execution_result}")

    def export(self):
        output_path = PROJECT_DIR / "melange_result.json"
        with open(output_path, "w") as f:
            json.dump(self.execution_result, f, indent=4)

        print(f"[Melange] Output saved to {output_path}")

