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

import asyncio
import enum
import functools
from abc import ABC, abstractmethod
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Sequence

from .status import Status
from .utils import cpu_perf_timer, human_readable_bytes

TOKEN_BUCKETS = [1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8096]
MS_BUCKETS = [
    1,
    2,
    3,
    5,
    8,
    10,
    20,
    30,
    50,
    80,
    100,
    200,
    300,
    500,
    800,
    1000,
    2000,
    3000,
]


@dataclass
class Breakdowns:
    breakdowns: OrderedDict = field(default_factory=OrderedDict)

    def add(self, name: str, value: int) -> None:
        """Add a breakdown stage.
        Args:
            name: The name of the breakdown stage.
            value: The value of the breakdown stage.
        """
        assert name not in self.breakdowns, f"Breakdown {name} already exists"
        self.breakdowns[name] = value


class Metrics(ABC):
    """Metrics API"""

    @abstractmethod
    def reset(self) -> None:
        """Reset the metrics."""
        raise NotImplementedError

    @abstractmethod
    def summary(self) -> str:
        raise NotImplementedError


class MetricsExporter(ABC):
    """Metrics exporter API."""

    @abstractmethod
    def export(self, labels: Dict[str, str], metrics: Metrics) -> None:
        raise NotImplementedError


class BaseMetricsExporter(MetricsExporter):
    """Base metrics exporter."""

    _prefix: str
    _labelnames: List[str]
    _counter_cls: Any = None
    _gauge_cls: Any = None
    _histogram_cls: Any = None

    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        """Register the metrics exporter."""
        self._prefix = prefix
        self._labelnames = labelnames
        self._counter_cls = counter_cls
        self._gauge_cls = gauge_cls
        self._histogram_cls = histogram_cls

    def _export_counter(
        self,
        counter,
        labels: Dict[str, str],
        data: int | float,
    ) -> None:
        if data < 0:
            return
        counter.labels(**labels).inc(data)

    def _export_counter_set(
        self,
        counter,
        labels: Dict[str, str],
        data: Dict[str, int] | Dict[str, float],
        set_labelname: str,
    ) -> None:
        for label, count in data.items():
            counter.labels(**{**labels, set_labelname: label}).inc(count)

    def _export_gauge(
        self, gauge, labels: Dict[str, str], data: int | float
    ) -> None:
        gauge.labels(**labels).set(data)

    def _export_histogram(
        self,
        histogram,
        labels: Dict[str, str],
        data: List[int] | List[float],
    ) -> None:
        for datum in data:
            histogram.labels(**labels).observe(datum)


class MetricRecorder(ABC):
    """Metric recorder API."""

    class OP(enum.Enum):
        PUT = enum.auto()
        GET = enum.auto()
        ACQUIRE = enum.auto()
        EXISTS = enum.auto()

    class Resource(enum.Enum):
        L1_EVICTION_POLICY = enum.auto()
        L1_ALLOCATOR = enum.auto()

    @abstractmethod
    def record(
        self,
        op: OP,
        num_prefix: int,
        num_tokens: int,
        status: Status,
        lat_ms: int,
        breakdowns: Breakdowns | None = None,
    ) -> None:
        """Record the metrics.
        Args:
            op: The operation.
            num_prefix: The number of prefix tokens.
            num_tokens: The number of tokens.
            status: The status of the operation.
            lat_ms: The duration of the operation in milliseconds.
            breakdowns: The breakdowns of the operation.
        """
        raise NotImplementedError


class OpMetrics(Metrics):
    """Op metrics."""

    num_ops: int = 0
    num_errors: int = 0
    num_errors_by_reason: Dict[str, int] = {}
    num_prefixes: List[int] = []
    num_tokens: List[int] = []
    num_fetched_tokens: List[int] = []
    op_lat_ms: List[int]
    op_breadowns: List[Breakdowns]

    def __init__(
        self,
        op: MetricRecorder.OP,
        is_error: Callable[[Status], bool],
        block_ntokens: int,
        enable_time_measurement: bool = True,
        enable_breakdown_measurement: bool = True,
    ) -> None:
        self._op = op
        self._is_error = is_error
        self._block_ntokens = block_ntokens
        self._enable_time_measurement = enable_time_measurement
        self._enable_breakdown_measurement = enable_breakdown_measurement
        self._init_optionals()

    def _init_optionals(self) -> None:
        if self._enable_time_measurement:
            self.op_lat_ms = []
        if self._enable_breakdown_measurement:
            self.op_breadowns = []

    @property
    def time_measurement_enabled(self) -> bool:
        return self._enable_time_measurement

    @property
    def breakdown_measurement_enabled(self) -> bool:
        return self._enable_breakdown_measurement

    def _is_get(self) -> bool:
        return self._op in [MetricRecorder.OP.GET, MetricRecorder.OP.ACQUIRE]

    def add(
        self,
        num_prefix: int,
        num_tokens: int,
        status: Status,
        lat_ms: int,
        breakdowns: Breakdowns | None = None,
    ) -> None:
        self.num_ops += 1
        if self._is_error(status):
            self.num_errors += 1
            error_key = status.error_code.name.lower()
            prev = self.num_errors_by_reason.get(error_key, 0)
            self.num_errors_by_reason[error_key] = prev + 1
        elif self._is_get() and status.is_ok():
            value = status.get()
            if isinstance(value, tuple):
                num_fetched_tokens = value[0]
            elif isinstance(value, int):
                num_fetched_tokens = value * self._block_ntokens
            else:
                num_fetched_tokens = len(value) * self._block_ntokens
            self.num_fetched_tokens.append(num_fetched_tokens)
        self.num_prefixes.append(num_prefix)
        self.num_tokens.append(num_tokens)
        if self._enable_time_measurement:
            self.op_lat_ms.append(lat_ms)
        if self._enable_breakdown_measurement and breakdowns is not None:
            self.op_breadowns.append(breakdowns)
            assert len(self.op_breadowns) == self.num_ops

    def reset(self) -> None:
        self.num_errors_by_reason.clear()
        self.num_prefixes = []
        self.num_tokens = []
        self.num_fetched_tokens = []
        self._init_optionals()

    def summary(self) -> str:
        iter_len = len(self.num_prefixes)
        total_prefixes = sum(self.num_prefixes)
        avg_prefixes = total_prefixes / iter_len if iter_len > 0 else 0
        total_tokens = sum(self.num_tokens)
        avg_tokens = total_tokens / iter_len if iter_len > 0 else 0
        total_fetched_tokens = sum(self.num_fetched_tokens)
        avg_fetched_tokens = (
            total_fetched_tokens / iter_len if iter_len > 0 else 0
        )
        summary = (
            f"{self._op.name}: Num. of ops: {self.num_ops}, "
            f"Num. of prefixes (iter): total={total_prefixes}, "
            f"avg={avg_prefixes:.2f}, "
            f"Num. of tokens (iter): total={total_tokens}, "
            f"avg={avg_tokens:.2f}"
        )
        if self._is_get():
            summary += (
                f", Num. of fetched (iter): "
                f"total={total_fetched_tokens}, "
                f"avg={avg_fetched_tokens:.2f}"
            )
        if self._enable_time_measurement:
            total_lat_ms = sum(self.op_lat_ms)
            avg_lat_ms = total_lat_ms / iter_len if iter_len > 0 else 0
            summary += (
                f", Latency (iter, ms): total={total_lat_ms:.2f}, "
                f"avg={avg_lat_ms:.2f}"
            )
        if self.num_errors > 0:
            summary += f", Num. of errors: {self.num_errors}"
        return summary


class OpMetricsExporter(BaseMetricsExporter):
    """Op metrics exporter."""

    OP_TYPE_LABELNAME = "op_type"
    REASON_LABELNAME = "reason"

    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        labelnames = labelnames.copy() or []
        labelnames.append(self.OP_TYPE_LABELNAME)

        super().__init__(
            prefix=prefix,
            labelnames=labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

        self._init_exporter_fields()

    def _init_exporter_fields(self) -> None:
        self.counter_num_ops = self._counter_cls(
            name=f"{self._prefix}num_ops",
            documentation="Cumulative number of operations.",
            labelnames=self._labelnames,
        )
        self.counter_num_errors = self._counter_cls(
            name=f"{self._prefix}num_errors",
            documentation="Cumulative number of errors.",
            labelnames=self._labelnames + [self.REASON_LABELNAME],
        )
        self.histogram_iteration_prefixes = self._histogram_cls(
            name=f"{self._prefix}iteration_prefixes",
            documentation="Histogram of number of prefixes per iteration.",
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_tokens = self._histogram_cls(
            name=f"{self._prefix}iteration_tokens",
            documentation="Histogram of number of tokens per iteration.",
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_fetched_tokens = self._histogram_cls(
            name=f"{self._prefix}iteration_fetched_tokens",
            documentation=(
                "Histogram of number of fetched tokens " "per iteration."
            ),
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_op_lat_ms = self._histogram_cls(
            name=f"{self._prefix}iteration_op_lat_ms",
            documentation=(
                "Histogram of operation latencies " "per iteration in ms."
            ),
            labelnames=self._labelnames,
            buckets=MS_BUCKETS,
        )

    def export(self, labels: Dict[str, str], metrics: Metrics) -> None:
        assert isinstance(metrics, OpMetrics)

        if metrics.num_ops <= 0:
            return

        labels = labels.copy()
        labels[self.OP_TYPE_LABELNAME] = metrics._op.name.lower()
        assert set(labels.keys()) == set(self._labelnames), (
            f"Labels " f"{set(labels.keys())} do not match {self._labelnames}"
        )

        self._export_counter(
            self.counter_num_ops, labels, len(metrics.num_prefixes)
        )
        self._export_counter_set(
            self.counter_num_errors,
            labels,
            metrics.num_errors_by_reason,
            self.REASON_LABELNAME,
        )
        self._export_histogram(
            self.histogram_iteration_prefixes, labels, metrics.num_prefixes
        )
        self._export_histogram(
            self.histogram_iteration_tokens, labels, metrics.num_tokens
        )
        if metrics._is_get():
            self._export_histogram(
                self.histogram_iteration_fetched_tokens,
                labels,
                metrics.num_fetched_tokens,
            )
        if metrics._enable_time_measurement:
            self._export_histogram(
                self.histogram_iteration_op_lat_ms, labels, metrics.op_lat_ms
            )


class UsageMetrics(Metrics):
    """Usage metrics."""

    resource: MetricRecorder.Resource
    capacity_nbytes: int = 0
    used_nbytes: int = 0

    def __init__(
        self,
        resource: MetricRecorder.Resource,
        capacity_nbytes: int,
    ) -> None:
        self.resource = resource
        self.capacity_nbytes = capacity_nbytes

    def update(self, used_nbytes: int) -> None:
        self.used_nbytes = used_nbytes

    def reset(self) -> None:
        pass

    def summary(self) -> str:
        return (
            f"{self.resource.name}: "
            f"{human_readable_bytes(self.used_nbytes)}"
            f"/{human_readable_bytes(self.capacity_nbytes)}"
        )


class UsageMetricsExporter(BaseMetricsExporter):
    """Usage metrics exporter."""

    RESOURCE_TYPE_LABELNAME = "resource_type"

    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        labelnames = labelnames.copy() or []
        labelnames.append(self.RESOURCE_TYPE_LABELNAME)
        super().__init__(
            prefix=prefix,
            labelnames=labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )
        self._init_exporter_fields()

    def _init_exporter_fields(self) -> None:
        self.gauge_used = self._gauge_cls(
            name=f"{self._prefix}used",
            documentation="Used resource.",
            labelnames=self._labelnames,
        )
        self.gauge_capacity = self._gauge_cls(
            name=f"{self._prefix}capacity",
            documentation="Capacity.",
            labelnames=self._labelnames,
        )

    def export(self, labels: Dict[str, str], metrics: Metrics) -> None:
        assert isinstance(metrics, UsageMetrics)

        if metrics.capacity_nbytes <= 0:
            return

        labels = labels.copy()
        labels[self.RESOURCE_TYPE_LABELNAME] = metrics.resource.name.lower()
        assert set(labels.keys()) == set(self._labelnames), (
            f"Labels " f"{set(labels.keys())} do not match {self._labelnames}"
        )

        self._export_gauge(self.gauge_used, labels, metrics.used_nbytes)
        self._export_gauge(self.gauge_capacity, labels, metrics.capacity_nbytes)


class BaseCacheMetrics(Metrics, MetricRecorder):
    """The base metrics of a cache."""

    put_metrics: OpMetrics
    get_metrics: OpMetrics
    exists_metrics: OpMetrics
    num_tokens_hit: int = 0
    num_tokens_miss: int = 0

    def __init__(
        self,
        *,
        cache_type: str,
        block_ntokens: int,
        enable_time_measurement: bool = True,
        enable_breakdown_measurement: bool = True,
    ) -> None:
        self._cache_type = cache_type
        self._block_ntokens = block_ntokens
        self._enable_time_measurement = enable_time_measurement
        self._enable_breakdown_measurement = enable_breakdown_measurement
        self.put_metrics = OpMetrics(
            MetricRecorder.OP.PUT,
            lambda status: not status.is_ok(),
            block_ntokens,
            enable_time_measurement,
            enable_breakdown_measurement,
        )
        self.get_metrics = OpMetrics(
            MetricRecorder.OP.GET,
            lambda status: not status.is_ok() and not status.is_not_found(),
            block_ntokens,
            enable_time_measurement,
            enable_breakdown_measurement,
        )
        self.exists_metrics = OpMetrics(
            MetricRecorder.OP.EXISTS,
            lambda status: not status.is_ok() and not status.is_not_found(),
            block_ntokens,
            enable_time_measurement,
            enable_breakdown_measurement,
        )

    @property
    def time_measurement_enabled(self) -> bool:
        return self._enable_time_measurement

    @property
    def breakdown_measurement_enabled(self) -> bool:
        return self._enable_breakdown_measurement

    def _get_all_metrics(self) -> List[Metrics]:
        return [self.put_metrics, self.get_metrics, self.exists_metrics]

    def _agg_hit_miss(self, num_tokens: int, status: Status) -> None:
        if status.is_ok():
            value = status.get()
            if isinstance(value, tuple):
                num_tokens_hit = value[0]
            elif isinstance(value, int):
                num_tokens_hit = value * self._block_ntokens
            else:
                num_tokens_hit = len(value) * self._block_ntokens
            self.num_tokens_hit += num_tokens_hit
            self.num_tokens_miss += num_tokens - num_tokens_hit
        else:
            self.num_tokens_miss += num_tokens

    def record(
        self,
        op: MetricRecorder.OP,
        num_prefix: int,
        num_tokens: int,
        status: Status,
        lat_ms: int,
        breakdowns: Breakdowns | None = None,
    ) -> None:
        if op is MetricRecorder.OP.PUT:
            self.put_metrics.add(
                num_prefix, num_tokens, status, lat_ms, breakdowns
            )
        elif op is MetricRecorder.OP.GET:
            self.get_metrics.add(
                num_prefix, num_tokens, status, lat_ms, breakdowns
            )
            self._agg_hit_miss(num_tokens, status)
        elif op is MetricRecorder.OP.EXISTS:
            self.exists_metrics.add(
                num_prefix, num_tokens, status, lat_ms, breakdowns
            )
        else:
            raise ValueError(f"Unknown op: {op}")

    def reset(self):
        self.put_metrics.reset()
        self.get_metrics.reset()
        self.exists_metrics.reset()

    def summary(self) -> str:
        summary = ""
        for m in [self.put_metrics, self.exists_metrics, self.get_metrics]:
            if m.num_ops > 0:
                summary += ", " if len(summary) > 0 else ""
                summary += f"{m.summary()}"
        if self.get_metrics.num_ops > 0:
            summary += ", " if len(summary) > 0 else ""
            hit_rate = (
                self.num_tokens_hit
                * 100
                / (self.num_tokens_hit + self.num_tokens_miss)
            )
            summary += (
                f"Num. of hit: {self.num_tokens_hit}, "
                f"Num. of miss: {self.num_tokens_miss}, "
                f"Hit rate: {hit_rate:.2f}%"
            )
        if len(summary) > 0:
            summary = f"{self._cache_type}: {summary}"
        return summary


class BaseCacheMetricsExporter(BaseMetricsExporter):
    """The base metrics exporter of a cache."""

    CACHE_TYPE_LABELNAME = "cache_type"

    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        labelnames = labelnames.copy() or []
        labelnames.append(self.CACHE_TYPE_LABELNAME)
        super().__init__(
            prefix=prefix,
            labelnames=labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

        self.op_metrics_exporter = OpMetricsExporter(
            prefix=prefix,
            labelnames=self._labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

        self.usage_metrics_exporter = UsageMetricsExporter(
            prefix=prefix,
            labelnames=self._labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

    def export(self, labels: Dict[str, str], metrics: Metrics) -> None:
        assert isinstance(metrics, BaseCacheMetrics)
        labels = labels.copy()
        labels[self.CACHE_TYPE_LABELNAME] = metrics._cache_type.lower()
        assert set(labels.keys()) == set(self._labelnames), (
            f"Labels " f"{set(labels.keys())} do not match {self._labelnames}"
        )

        for m in metrics._get_all_metrics():
            if isinstance(m, OpMetrics):
                self.op_metrics_exporter.export(labels, m)
            else:
                self.usage_metrics_exporter.export(labels, m)


class L1CacheMetrics(BaseCacheMetrics):
    """The metrics of the l1 cache."""

    acquire_metrics: OpMetrics
    eviction_policy_usage_metrics: UsageMetrics
    allocator_usage_metrics: UsageMetrics

    def __init__(
        self,
        *,
        cache_type: str,
        block_ntokens: int,
        capacity_nbytes: int = 0,
        enable_time_measurement: bool = True,
        enable_breakdown_measurement: bool = True,
    ) -> None:
        super().__init__(
            cache_type=cache_type,
            block_ntokens=block_ntokens,
            enable_time_measurement=enable_time_measurement,
            enable_breakdown_measurement=enable_breakdown_measurement,
        )
        self.acquire_metrics = OpMetrics(
            MetricRecorder.OP.ACQUIRE,
            lambda status: not status.is_ok() and not status.is_not_found(),
            block_ntokens,
            enable_time_measurement,
            enable_breakdown_measurement,
        )
        self.eviction_policy_usage_metrics = UsageMetrics(
            MetricRecorder.Resource.L1_EVICTION_POLICY,
            capacity_nbytes,
        )
        self.allocator_usage_metrics = UsageMetrics(
            MetricRecorder.Resource.L1_ALLOCATOR,
            capacity_nbytes,
        )

    def _get_all_metrics(self) -> List[Metrics]:
        return [
            self.put_metrics,
            self.get_metrics,
            self.exists_metrics,
            self.acquire_metrics,
            self.eviction_policy_usage_metrics,
            self.allocator_usage_metrics,
        ]

    def record(
        self,
        op: MetricRecorder.OP,
        num_prefix: int,
        num_tokens: int,
        status: Status,
        lat_ms: int,
        breakdowns: Breakdowns | None = None,
    ) -> None:
        if op is MetricRecorder.OP.ACQUIRE:
            self.acquire_metrics.add(
                num_prefix, num_tokens, status, lat_ms, breakdowns
            )
            self._agg_hit_miss(num_tokens, status)
        else:
            super().record(
                op, num_prefix, num_tokens, status, lat_ms, breakdowns
            )

    def trace_usage(self, resource, used_nbytes):
        if resource is MetricRecorder.Resource.L1_EVICTION_POLICY:
            self.eviction_policy_usage_metrics.update(used_nbytes)
        elif resource is MetricRecorder.Resource.L1_ALLOCATOR:
            self.allocator_usage_metrics.update(used_nbytes)
        else:
            raise ValueError(f"Unknown resource: {resource}")

    def reset(self):
        super().reset()
        self.acquire_metrics.reset()

    def summary(self) -> str:
        summary = ""
        for m in [
            self.put_metrics,
            self.exists_metrics,
            self.get_metrics,
            self.acquire_metrics,
        ]:
            if m.num_ops > 0:
                summary += ", " if len(summary) > 0 else ""
                summary += f"{m.summary()}"
        if self.get_metrics.num_ops + self.acquire_metrics.num_ops > 0:
            summary += ", " if len(summary) > 0 else ""
            hit_rate = (
                self.num_tokens_hit
                * 100
                / (self.num_tokens_hit + self.num_tokens_miss)
            )
            summary += (
                f"Num. of hit: {self.num_tokens_hit}, "
                f"Num. of miss: {self.num_tokens_miss}, "
                f"Hit rate: {hit_rate:.2f}%"
            )
        for r in [
            self.eviction_policy_usage_metrics,
            self.allocator_usage_metrics,
        ]:
            if r.capacity_nbytes > 0:
                summary += ", " if len(summary) > 0 else ""
                summary += f"{r.summary()}"
        if len(summary) > 0:
            summary = f"{self._cache_type}: {summary}"
        return summary


L2CacheMetrics = BaseCacheMetrics
CacheMgrMetrics = L1CacheMetrics


class KVCacheMetrics(Metrics):
    """The metrics of the kv cache.
    Args:
        l1: The metrics of the l1 cache.
        l2: The metrics of the l2 cache.
        mgr: The metrics of the cache mgr.
    """

    l1: L1CacheMetrics | None
    l2: L2CacheMetrics | None
    mgr: CacheMgrMetrics

    def __init__(
        self,
        *,
        block_ntokens: int,
        capacity_nbytes: int,
        enable_l1: bool,
        enable_l2: bool,
        enable_time_measurement: bool = True,
        enable_breakdown_measurement: bool = True,
    ) -> None:
        self.l1 = None
        self.l2 = None

        if enable_l1:
            self.l1 = L1CacheMetrics(
                cache_type="L1Cache",
                block_ntokens=block_ntokens,
                capacity_nbytes=capacity_nbytes,
                enable_time_measurement=enable_time_measurement,
                enable_breakdown_measurement=enable_breakdown_measurement,
            )
        if enable_l2:
            self.l2 = L2CacheMetrics(
                cache_type="L2Cache",
                block_ntokens=block_ntokens,
                enable_time_measurement=enable_time_measurement,
                enable_breakdown_measurement=enable_breakdown_measurement,
            )
        self.mgr = CacheMgrMetrics(
            cache_type="CacheMgr",
            block_ntokens=block_ntokens,
            enable_time_measurement=enable_time_measurement,
            enable_breakdown_measurement=enable_breakdown_measurement,
        )

    @property
    def time_measurement_enabled(self) -> bool:
        return self.mgr.time_measurement_enabled

    def reset(self):
        if self.l1 is not None:
            self.l1.reset()
        if self.l2 is not None:
            self.l2.reset()
        self.mgr.reset()

    def summary(self):
        summary = self.mgr.summary()
        if len(summary) > 0:
            summary = f"{summary}"

        if self.l1 is not None:
            l1_summary = self.l1.summary()
            if len(l1_summary) > 0:
                summary += "\n" if len(summary) > 0 else ""
                summary += f"\t{l1_summary}"
        if self.l2 is not None:
            l2_summary = self.l2.summary()
            if len(l2_summary) > 0:
                summary += "\n" if len(summary) > 0 else ""
                summary += f"\t{l2_summary}"
        return summary


class KVCacheMetricsExporter(BaseCacheMetricsExporter):
    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        super().__init__(
            prefix=f"{prefix}kv_cache_ol_",
            labelnames=labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

    def export(self, labels: Dict[str, str], metrics: Metrics) -> None:
        assert isinstance(metrics, KVCacheMetrics)
        if metrics.l1 is not None:
            super().export(labels, metrics.l1)
        if metrics.l2 is not None:
            super().export(labels, metrics.l2)
        super().export(labels, metrics.mgr)


class MeasurableBase:
    def __init__(
        self,
        recorder: BaseCacheMetrics | None,
    ):
        self._recorder = recorder
        self._enable_time_measurement = (
            recorder is not None and recorder.time_measurement_enabled
        )
        self._enable_breakdown_measurement = (
            recorder is not None and recorder.breakdown_measurement_enabled
        )

    @staticmethod
    def measure(op: MetricRecorder.OP):
        """A decorator that measures the metrics of an operation."""

        def measure_wrap(func):
            @functools.wraps(func)
            async def async_wrapper(
                self,
                prefix: Sequence[int] | None,
                tokens: Sequence[int],
                *args,
                **kwargs,
            ):
                with cpu_perf_timer(
                    self._enable_time_measurement
                ) as get_lat_ms:
                    status = await func(self, prefix, tokens, *args, **kwargs)
                if self._recorder:
                    self._recorder.record(
                        op,
                        0 if prefix is None else len(prefix),
                        len(tokens),
                        status,
                        get_lat_ms(),
                    )
                return status

            @functools.wraps(func)
            def wrapper(
                self,
                prefix: Sequence[int] | None,
                tokens: Sequence[int],
                *args,
                **kwargs,
            ):
                with cpu_perf_timer(
                    self._enable_time_measurement
                ) as get_lat_ms:
                    status = func(self, prefix, tokens, *args, **kwargs)
                if self._recorder:
                    self._recorder.record(
                        op,
                        0 if prefix is None else len(prefix),
                        len(tokens),
                        status,
                        get_lat_ms(),
                    )
                return status

            if asyncio.iscoroutinefunction(func):
                return async_wrapper
            else:
                return wrapper

        return measure_wrap
