from typing import Protocol, Tuple
from dataclasses import dataclass, field

@dataclass
class GPUProfile:
    """Support json input like:
    {
        "gpu": "A10",
        "cost": 1.01,
        "tputs": [[2, 1], [5, 2]]
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

class WorkloadProfile(Protocol):
    """Description of worklaod characteristic"""

    @property
    def signature() -> Tuple[float]:
        """The signature of the workload"""

    @property
    def rate() -> float:
        """The request rate of the workload in the unit RPS"""