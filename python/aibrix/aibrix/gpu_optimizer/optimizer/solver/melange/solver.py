import pulp
from pulp import LpInteger, LpMinimize, LpProblem, LpVariable

from .util import tputs_to_loads_2d


# This class is adapted from code originally written by Tyler Griggs
# found at https://github.com/tyler-griggs/melange-release
# See: https://tyler-griggs.github.io/blogs/melange
class Solver:
    def __init__(
        self, workload_distribution: list, total_request_rate: float, gpu_info: dict
    ):
        self.workload_distribution = workload_distribution
        self.overall_rate = total_request_rate
        self.gpu_info = gpu_info

    def run(self, logs=False):
        raise NotImplementedError


class MelangeSolver(Solver):
    def __init__(
        self,
        workload_distribution: list,
        total_request_rate: float,
        gpu_info: dict,
        slice_factor: int,
    ):
        super().__init__(workload_distribution, total_request_rate, gpu_info)
        self.slice_factor = slice_factor

    def run(self, logs=False):
        # Multiply overall rate across distribution.
        request_rate_histogram = []
        for i in range(len(self.workload_distribution)):
            request_rate_histogram.append([])
            for j in range(len(self.workload_distribution[0])):
                request_rate_histogram[-1].append(
                    self.workload_distribution[i][j] * self.overall_rate
                )

        # Convert the profiled max throughputs into mapping from request size to load
        for gpu in self.gpu_info:
            self.gpu_info[gpu]["loads"] = tputs_to_loads_2d(self.gpu_info[gpu]["tputs"])

        gpu_types = list(self.gpu_info.keys())
        cost_vector = [self.gpu_info[gpu]["cost"] for gpu in gpu_types]

        # Create slices, which is a single dimension.
        slices = []
        for i in range(len(request_rate_histogram)):
            for j in range(len(request_rate_histogram[i])):
                for _ in range(self.slice_factor):
                    slices.append(request_rate_histogram[i][j] / self.slice_factor)

        # Create slice-to-load mapping, which is a single dimension.
        for gpu in gpu_types:
            slice_loads = []
            for i in range(len(self.gpu_info[gpu]["loads"])):
                for j in range(len(self.gpu_info[gpu]["loads"][i])):
                    for _ in range(self.slice_factor):
                        slice_loads.append(self.gpu_info[gpu]["loads"][i][j])
            assert len(slices) == len(slice_loads)
            self.gpu_info[gpu]["slice_loads"] = slice_loads

        # Decision matrix value is binary. The slice is assigned to a GPU, or it isn't.
        matrix_rows = len(slices)
        matrix_cols = len(gpu_types)

        # Vector value is non-negative integer of how many of each GPU type are needed
        vector_length = matrix_cols

        decision_matrix = [
            [
                LpVariable(f"x_{i}_{j}", cat=LpInteger, lowBound=0, upBound=1)
                for j in range(matrix_cols)
            ]
            for i in range(matrix_rows)
        ]
        decision_vector = [
            LpVariable(f"y_{i}", cat=LpInteger, lowBound=0)
            for i in range(vector_length)
        ]

        # Objective: minimize cost
        problem = LpProblem("GpuAllocation", LpMinimize)
        problem += pulp.lpSum(
            [decision_vector[i] * cost_vector[i] for i in range(len(decision_vector))]
        )

        # C1: Each row of decision matrix must sum to exactly 1 (ie, each slice assigned to one GPU)
        for i in range(len(decision_matrix)):
            problem += pulp.lpSum(decision_matrix[i]) == 1

        # C2: Load of column of decision matrix must fit in decision vector capacity
        for j in range(len(decision_matrix[0])):
            # j is idx of GPU type, i is slice
            problem += (
                pulp.lpSum(
                    [
                        decision_matrix[i][j]
                        * self.gpu_info[gpu_types[j]]["slice_loads"][i]
                        * slices[i]
                        for i in range(len(decision_matrix))
                    ]
                )
                <= decision_vector[j]
            )

        # Solve the problem
        problem.solve(pulp.PULP_CBC_CMD(msg=0))

        # For Arm-based Mac platforms.
        # solver= pulp.getSolver('COIN_CMD', path='/opt/homebrew/opt/cbc/bin/cbc', msg=0)
        # problem.solve(solver)

        # Print the results if needed
        if logs:
            print("Decision Matrix:")
            for row in decision_matrix:
                print([var.value() for var in row])
            print("Decision Vector:")
            print(f"{[var.value() for var in decision_vector]}")

        if pulp.LpStatus[problem.status] != "Optimal":
            return None

        solution_dict = {}
        for i in range(len(decision_vector)):
            solution_dict[gpu_types[i]] = int(decision_vector[i].value())

        total_cost = 0
        for gpu in solution_dict:
            total_cost += solution_dict[gpu] * self.gpu_info[gpu]["cost"]
        solution_dict["cost"] = total_cost

        return solution_dict
