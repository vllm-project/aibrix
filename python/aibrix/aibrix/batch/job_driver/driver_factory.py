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

from typing import Optional

# Import for side effects: each provisioning backend registers its Runtime
# factory under its ComputeProvider key. External / noop register from
# runtime.base. (One import per provider module.)
import aibrix.batch.job_driver.runtime.k8s_deployment  # noqa: F401,E402  Kubernetes
import aibrix.batch.job_driver.runtime.k8s_job  # noqa: F401,E402  KubernetesJob
import aibrix.batch.job_driver.runtime.lambda_cloud  # noqa: F401,E402  LambdaCloud
import aibrix.batch.job_driver.runtime.runpod  # noqa: F401,E402  RunPod
from aibrix.batch.client import EndpointSource
from aibrix.batch.job_driver.base import BaseJobDriver
from aibrix.batch.job_driver.driver import JobDriver
from aibrix.batch.job_driver.runtime import (  # noqa: E402
    create_runtime,
    registered_runtimes,
)
from aibrix.batch.job_entity import (  # noqa: E402
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
)
from aibrix.batch.state import JobEntityManager, RunningJobs  # noqa: E402
from aibrix.context import InfrastructureContext  # noqa: E402
from aibrix.logger import init_logger  # noqa: E402

logger = init_logger(__name__)


def create_job_driver(
    context: InfrastructureContext,
    progress_manager: RunningJobs,
    entity_manager: Optional[JobEntityManager] = None,
    job: Optional[BatchJob] = None,
    endpoint_source: Optional[EndpointSource] = None,
) -> JobDriver:
    """One driver — ``BaseJobDriver`` — parameterized by a ``Runtime``.

    The Runtime is selected by the job's ``aibrix.compute.provider``; a new
    backend is a new registered Runtime, never a new driver. With no job or no
    compute provider (the standalone path), the injected endpoint source drives
    an ``External`` runtime.
    """
    provider = job.spec.compute_provider if job is not None else None
    job_id = getattr(job, "job_id", None)
    if provider is None:
        # Standalone / endpoint-source path: dispatch against the injected
        # source (possibly None for prepare/finalize-only) via External.
        runtime = create_runtime("External", endpoint_source=endpoint_source)
        logger.info(
            "Selected job runtime",
            job_id=job_id,
            provider=None,
            runtime=type(runtime).__name__,
            endpoint_source=type(endpoint_source).__name__
            if endpoint_source is not None
            else None,
        )  # type: ignore[call-arg]
        return BaseJobDriver(progress_manager, runtime)

    try:
        runtime = create_runtime(
            provider,
            job=job,
            context=context,
            entity_manager=entity_manager,
            endpoint_source=endpoint_source,
        )
    except KeyError as exc:
        raise BatchJobError(
            BatchJobErrorCode.INVALID_DRIVER,
            f"Unknown compute provider '{provider}'; "
            f"registered: {registered_runtimes()}",
        ) from exc
    logger.info(
        "Selected job runtime",
        job_id=job_id,
        provider=provider,
        runtime=type(runtime).__name__,
        registered=registered_runtimes(),
    )  # type: ignore[call-arg]
    return BaseJobDriver(progress_manager, runtime)
