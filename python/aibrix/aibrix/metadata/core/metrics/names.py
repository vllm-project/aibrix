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

"""Canonical metadata-service metric names.

Each name is kept here so instrumentation sites and dashboards stay aligned.
"""

# Count of inbound metadata HTTP requests.
METRIC_METADATA_HTTP_REQUEST = "metadata.http.request"

# Latency of inbound metadata HTTP requests, emitted in milliseconds.
METRIC_METADATA_HTTP_DURATION = "metadata.http.duration"

# Count of accepted batch job create requests at the API boundary.
METRIC_METADATA_BATCH_API_JOB_INCOMING = "metadata.batch.api.job.incoming"

# Count of finalized jobs at the API/status boundary, tagged by finalize type and error code.
METRIC_METADATA_BATCH_API_JOB_FINISHED = "metadata.batch.api.job.finished"

# End-to-end execution time from in-progress to terminal state, emitted in milliseconds.
METRIC_METADATA_BATCH_API_JOB_EXECUTION_TIME = "metadata.batch.api.job.execution_time"

# Time spent in each execution phase, tagged by `phase`, emitted in milliseconds.
METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME = (
    "metadata.batch.api.job.execution_phase_time"
)

# Count of batch requests as they finish in the driver, tagged by result.
# Use rate() on this counter to derive completion throughput over time.
METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_COMPLETED = (
    "metadata.batch.driver.request.completed"
)

# Token usage reported when a driver request finishes, tagged by token type.
# Use rate() on this counter to derive per-second token throughput.
METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_USAGE_TOKENS = (
    "metadata.batch.driver.request.usage.tokens"
)

# Cached input tokens reported when a driver request finishes.
# Use rate() on this counter to derive per-second cached-token throughput.
METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_CACHED_TOKENS = (
    "metadata.batch.driver.request.cached_tokens"
)

# Reasoning output tokens reported when a driver request finishes.
# Use rate() on this counter to derive per-second reasoning-token throughput.
METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_REASONING_TOKENS = (
    "metadata.batch.driver.request.reasoning_tokens"
)

# Count of failure injection events, tagged by injection type, breakpoint, and action.
METRIC_METADATA_BATCH_DRIVER_FAILURE_INJECTION = (
    "metadata.batch.driver.failure_injection"
)

# Count of runtimes that successfully start, tagged by runtime type.
METRIC_METADATA_BATCH_RUNTIME_STARTED = "metadata.batch.runtime.started"

# Count of runtimes that are torn down, tagged by runtime type.
METRIC_METADATA_BATCH_RUNTIME_TORN_DOWN = "metadata.batch.runtime.torn_down"

# TODO: implement runtime liveness via an independent reconciler instead of
# session-local bookkeeping. The planned design is:
# 1. register each started runtime in a shared registry
# 2. run a single coroutine that periodically checks liveness for all registered runtimes
# 3. reconcile the currently live set into this gauge
# This deferred design should emit the reconciled running runtime count, tagged
# by runtime type.
METRIC_METADATA_BATCH_RUNTIME_RUNNING = "metadata.batch.runtime.running"

# Latency of top-level JobStore operations, emitted in milliseconds.
METRIC_METADATA_JOB_STORE_DURATION = "metadata.job_store.duration"

# Count of logical JobStore operations such as get_job or update_job_status.
METRIC_METADATA_JOB_STORE_OPERATION = "metadata.job_store.operation"

# Count of underlying backend operations performed while serving a JobStore call.
METRIC_METADATA_JOB_STORE_STORAGE_OPERATIONS = "metadata.job_store.storage.operations"

# Count of metadata store failures.
METRIC_METADATA_STORE_ERROR = "metadata.store.error"

# Latency of metadata store operations, emitted in milliseconds.
METRIC_METADATA_STORE_DURATION = "metadata.store.duration"
