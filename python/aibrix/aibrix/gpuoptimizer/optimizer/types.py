from typing import Protocol, Tuple, List, Callable, Optional
from dataclasses import dataclass, field

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
    tputs: list = field(default_factory=list) # units: requests per second
    indexes: list = field(default_factory=list) # value ticks of tputs columns and rows

WorkloadSignatureErrorHandler = Callable[[int, float, float, float, float], None]
"""A function to handle the error with parameters(dimension, value, index assigned, value of index, value offset)."""


class WorkloadProfile(Protocol):
    """Description of worklaod characteristic"""
    def get_signature(self, indexes: List[List[float]], error_suppressor: Optional[WorkloadSignatureErrorHandler]=None) -> Tuple[int]:
        """Generate the index signature of the WorkloadProfile within the indexes' range.

        Args:
            indexes: A list of list of float, each list is a range of values.
            error_suppressor: A callback to suppress possible error. If None, raise an exception.
        """

    @property
    def signature() -> Tuple[int]:
        """The signature of the workload"""

    @property
    def rate() -> float:
        """The request rate of the workload in the unit RPS"""