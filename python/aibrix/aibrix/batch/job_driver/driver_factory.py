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

import aibrix.batch.constant as constant
from aibrix.batch.job_driver.local_driver import InferenceEngineClient
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    JobEntityManager,
)
from aibrix.context import InfrastructureContext

from .deployment_driver import DeploymentJobDriver
from .driver import JobDriver
from .local_driver import LocalJobDriver
from .progress_manager import JobProgressManager
from .simple_driver import SimpleJobDriver


def create_job_driver(
    context: InfrastructureContext,
    progress_manager: JobProgressManager,
    entity_manager: Optional[JobEntityManager] = None,
    job: Optional[BatchJob] = None,
    inference_client: Optional[InferenceEngineClient] = None,
) -> JobDriver:
    if job is None:
        return LocalJobDriver(progress_manager, inference_client)

    planner_decision = (
        job.spec.aibrix.planner_decision if job.spec.aibrix is not None else None
    )
    resource_details = (
        planner_decision.resource_details if planner_decision is not None else None
    )
    if resource_details:
        resource = resource_details[0]
        if (
            resource.resource_type == constant.BATCH_RESOURCE_TYPE_DEPLOYMENT
            and entity_manager is not None
            and job.spec.model_template_name is not None
        ):
            return DeploymentJobDriver(
                context,
                progress_manager,
                entity_manager,
            )

    if entity_manager is not None and entity_manager.is_scheduler_enabled():
        return SimpleJobDriver(progress_manager, entity_manager)

    if inference_client is not None:
        return LocalJobDriver(progress_manager, inference_client)

    raise BatchJobError(
        BatchJobErrorCode.INVALID_DRIVER,
        "No job driver avaiable, please specify AibrixMetadata, JobEntityManager with scheduling, or INFERENCE_ENGINE_ENDPOINT(env).",
    )
