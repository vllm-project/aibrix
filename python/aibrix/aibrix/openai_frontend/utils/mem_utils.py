# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: Copyright contributors to the vLLM project

"""GPU memory profiling utilities, ported from vLLM."""

import contextlib
import gc
import logging
import time
from collections.abc import Generator
from dataclasses import dataclass, field

import torch

logger = logging.getLogger(__name__)

GiB_bytes: int = 1 << 30
MiB_bytes: int = 1 << 20
KiB_bytes: int = 1 << 10


def format_mib(b: int) -> str:
    return f"{round(b / MiB_bytes, 2)}"


def format_gib(b: int) -> str:
    return f"{round(b / GiB_bytes, 2)}"


def gpu_memory_total(device: torch.device | None = None) -> int:
    """Return total CUDA memory in bytes."""
    _, total = torch.cuda.mem_get_info(device)
    return total


@dataclass
class MemorySnapshot:
    """Point-in-time snapshot of CUDA memory usage."""

    torch_peak: int = 0
    free_memory: int = 0
    total_memory: int = 0
    cuda_memory: int = 0
    torch_memory: int = 0
    non_torch_memory: int = 0
    timestamp: float = 0.0

    device: torch.device | None = None
    auto_measure: bool = True

    def __post_init__(self) -> None:
        if self.device is None:
            self.device_ = torch.device("cuda")
        else:
            self.device_ = torch.device(self.device)

        if self.auto_measure:
            self.measure()

    def measure(self) -> None:
        device = self.device_
        stats = torch.cuda.memory_stats(device)
        self.torch_peak = stats.get("allocated_bytes.all.peak", 0)
        self.free_memory, self.total_memory = torch.cuda.mem_get_info(device)
        self.cuda_memory = self.total_memory - self.free_memory
        self.torch_memory = torch.cuda.memory_reserved(device)
        self.non_torch_memory = self.cuda_memory - self.torch_memory
        self.timestamp = time.time()

    def __sub__(self, other: "MemorySnapshot") -> "MemorySnapshot":
        if self.device_ != other.device_:
            raise ValueError(
                f"Snapshots from different devices: {self.device_} vs {other.device_}"
            )
        return MemorySnapshot(
            torch_peak=self.torch_peak - other.torch_peak,
            free_memory=self.free_memory - other.free_memory,
            total_memory=self.total_memory - other.total_memory,
            cuda_memory=self.cuda_memory - other.cuda_memory,
            torch_memory=self.torch_memory - other.torch_memory,
            non_torch_memory=self.non_torch_memory - other.non_torch_memory,
            timestamp=self.timestamp - other.timestamp,
            device=self.device_,
            auto_measure=False,
        )

    def __repr__(self) -> str:
        return (
            f"torch_peak={format_gib(self.torch_peak)}GiB, "
            f"free={format_gib(self.free_memory)}GiB, "
            f"total={format_gib(self.total_memory)}GiB, "
            f"cuda={format_gib(self.cuda_memory)}GiB, "
            f"torch={format_gib(self.torch_memory)}GiB, "
            f"non_torch={format_gib(self.non_torch_memory)}GiB"
        )


class DeviceMemoryProfiler:
    """Context manager that measures GPU memory delta over a code block."""

    def __init__(self, device: torch.device | None = None):
        self.device = device

    def current_memory_usage(self) -> int:
        gc.collect()
        return torch.cuda.memory_allocated(self.device)

    def __enter__(self):
        self.initial_memory = self.current_memory_usage()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.final_memory = self.current_memory_usage()
        self.consumed_memory = self.final_memory - self.initial_memory
        gc.collect()


@dataclass
class MemoryProfilingResult:
    """Result of a memory profiling session. All numbers are in bytes."""

    non_kv_cache_memory: int = 0
    torch_peak_increase: int = 0
    non_torch_increase: int = 0
    weights_memory: int = 0
    before_create: MemorySnapshot = field(default_factory=MemorySnapshot)
    profile_time: float = 0.0

    def __post_init__(self) -> None:
        device = self.before_create.device_
        self.before_profile = MemorySnapshot(device=device, auto_measure=False)
        self.after_profile = MemorySnapshot(device=device, auto_measure=False)

    def __repr__(self) -> str:
        return (
            f"Memory profiling took {self.profile_time:.2f}s. "
            f"non_kv_cache={format_gib(self.non_kv_cache_memory)}GiB, "
            f"torch_peak_increase={format_gib(self.torch_peak_increase)}GiB, "
            f"non_torch_increase={format_gib(self.non_torch_increase)}GiB, "
            f"weights={format_gib(self.weights_memory)}GiB."
        )


@contextlib.contextmanager
def memory_profiling(
    baseline_snapshot: MemorySnapshot,
    weights_memory: int = 0,
    log_diff: bool = False,
) -> Generator[MemoryProfilingResult, None, None]:
    """Memory profiling context manager.

    Captures GPU memory usage before and after the wrapped code block and
    computes the peak increase in torch-allocated memory and non-torch memory.

    Args:
        baseline_snapshot: Memory snapshot taken before the profiled code.
        weights_memory: Memory used by model weights (bytes).
        log_diff: If True, log the before/after memory diff on exit.

    Usage::

        baseline = MemorySnapshot()
        with memory_profiling(baseline) as result:
            # ... run inference ...
        print(result.torch_peak_increase)
    """
    gc.collect()
    torch.cuda.empty_cache()
    torch.cuda.reset_peak_memory_stats(baseline_snapshot.device_)

    result = MemoryProfilingResult(
        before_create=baseline_snapshot,
        weights_memory=weights_memory,
    )

    result.before_profile.measure()

    yield result

    gc.collect()
    torch.cuda.empty_cache()

    result.after_profile.measure()

    diff_profile = result.after_profile - result.before_profile
    diff_from_create = result.after_profile - result.before_create
    result.torch_peak_increase = diff_profile.torch_peak
    result.non_torch_increase = diff_from_create.non_torch_memory
    result.profile_time = diff_profile.timestamp

    non_torch_memory = result.non_torch_increase
    peak_activation_memory = result.torch_peak_increase
    result.non_kv_cache_memory = (
        non_torch_memory + peak_activation_memory + result.weights_memory
    )

    if log_diff:
        logger.info(
            "memory_profiling: before=[%s] after=[%s] diff=[%s] took %.2fs",
            result.before_profile,
            result.after_profile,
            diff_profile,
            result.profile_time,
        )
