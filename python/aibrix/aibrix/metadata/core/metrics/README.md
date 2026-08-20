# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Metadata Metrics

This package provides the metadata-service metrics system used by
`aibrix.metadata.app`.

It has three goals:

- provide one small emission API for metadata code
- support multiple sinks at the same time
- keep metric names and Grafana queries aligned

## Package Layout

- `sink.py`: core sink abstraction, global `Emitter`, `Tag`, `T`, and `duration_ms`
- `setup.py`: bootstrap and shutdown helpers, sink assembly, Prometheus registry creation
- `prometheus.py`: Prometheus sink implementation
- `udp.py`: StatsD, Statsite, and DogStatsD sinks
- `bytedmetrics.py`: internal BytedMetrics sink
- `names.py`: canonical metric name catalog
- `tracking.py`: backend operation counting helpers used by JobStore read-amplification metrics

## How Setup Works

The metadata service initializes metrics during app startup in
`aibrix/metadata/app.py`.

The current flow is:

1. load `settings.METRICS`
2. call `setup_metrics(settings.METRICS)`
3. install the chosen sink(s) into the global `Emitter`
4. expose `/metrics` when Prometheus is enabled
5. call `shutdown_metrics()` during app shutdown

The relevant runtime object is `MetricsRuntime`:

```python
@dataclass
class MetricsRuntime:
    sink: Sink
    registry: CollectorRegistry | None = None
    metrics_app: ASGIApp | None = None
```

`setup_metrics()` may build a single sink or a `FanoutSink` if multiple sinks are
enabled at once.

## Configuration

Public metrics configuration lives in `aibrix/metadata/setting/metrics.py`.

Supported public environment variables:

- `METRICS_SERVICE_NAME`
- `METRICS_PROMETHEUS_ENABLED`
- `METRICS_STATSD_ADDR`
- `METRICS_STATSITE_ADDR`
- `METRICS_DOGSTATSD_ADDR`
- `METRICS_DOGSTATSD_TAGS`

If none of the public sinks are enabled, `load_metrics_config()` returns `None`
and the metadata service falls back to a `NoopSink`.

Internal provider-specific metrics config is loaded separately through
`aibrix.metadata.setting.InternalMetricsConfig`. This is how internal-only sinks
like BytedMetrics can be added without changing the OSS-facing `MetricsConfig`
shape.

## How To Emit Metrics

Import from `aibrix.metadata.core.metrics`:

```python
from aibrix.metadata.core.metrics import Emitter, T, duration_ms, metrics_names
```

### Counter

Use `Emitter.counter()` for values that accumulate over time.

```python
Emitter.counter(
    metrics_names.METRIC_METADATA_BATCH_API_JOB_INCOMING,
    1,
    T("endpoint", job.spec.endpoint),
)
```

### Gauge

Use `Emitter.gauge()` for a current level. A gauge sample should report the
current absolute value, not a delta.

```python
Emitter.gauge("metadata.example.inflight", inflight, T("worker", worker_id))
```

### Timer

Use `Emitter.timer()` when you already have the measured duration, or
`duration_ms()` when you start from `perf_counter()`.

```python
start = perf_counter()
...
duration_ms(
    Emitter,
    metrics_names.METRIC_METADATA_JOB_STORE_DURATION,
    start,
    T("operation", operation),
)
```

### Tags

Use `T(name, value)` to create tags. Tags should stay low-cardinality.

Good examples:

- endpoint name
- runtime type
- finalize type
- completion window
- token type

Avoid tags like:

- job id
- request body
- file id
- full error message

## Naming Rules

All canonical metric names must be defined in `names.py`.

Do not hardcode metric strings at emission sites. Add the constant in
`names.py`, document it there, then import it as `metrics_names.X`.

This keeps:

- instrumentation code aligned
- tests aligned
- Grafana queries aligned

## Prometheus And `/metrics`

When `METRICS_PROMETHEUS_ENABLED=true`, `setup_metrics()` creates a Prometheus
registry and the app exposes a `/metrics` endpoint.

Prometheus scrapes that endpoint and stores the time series. Grafana then reads
from Prometheus.

Metric names are written in code like:

- `metadata.batch.api.job.finished`

Prometheus will expose them with normalized names such as:

- `metadata_batch_api_job_finished_total`
- `metadata_batch_api_job_execution_time_ms_bucket`
- `metadata_batch_api_job_execution_phase_time_ms_bucket`

The dashboard queries must use the Prometheus-normalized names, not the raw
Python constants.

## Grafana Dashboard

The metadata dashboard lives at:

- `python/aibrix/observability/aibrix-metadata-grafana.json`

This file is part of the metrics system contract:

- the code defines the metric names and tags
- Prometheus exposes the normalized series
- Grafana queries and summarizes those series

When adding or changing metrics:

1. update `names.py`
2. update the emission site
3. update tests if needed
4. update `aibrix-metadata-grafana.json`

The dashboard currently focuses on summary views for:

- batch API intake and finalization
- batch execution breakdown across scheduling, runtime provision, task execution, and finalization
- request completion throughput
- request token throughput

Execution phase breakdown timers are emitted when each phase completes and carry
the base job labels plus a `phase` label. Current phases are:

- `scheduling`
- `runtime_provision`
- `task_execution`
- `finalization`

Batch job metrics derived from `tags_from_job()` now also include:

- `job_id`: the BatchJob status job ID
- `console_job_id`: `spec.aibrix.job_id` when present, otherwise `none`
- failure injection events
- runtime started and torn-down lifecycle counts
- JobStore latency and read amplification
