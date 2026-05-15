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

"""Runtime-side state projection for BatchJobs.

A ``JobStateRuntimeUpdater`` keeps a small, denormalized view of the
authoritative BatchJob document on whatever runtime executes the job
(K8s Job annotations, RunPod tags, EC2 tags, ...). The runtime view is
best-effort and operator-facing only; it is never read back as the
source of truth — the BatchJobStore remains authoritative.

Note: this layer is provisional. Once the provisioner abstraction
lands, naming and responsibilities here may shift to align with that
boundary.
"""

from aibrix.batch.runtime_updater.base import (
    JobStateRuntimeUpdater,
    NoOpJobStateRuntimeUpdater,
)

__all__ = [
    "JobStateRuntimeUpdater",
    "NoOpJobStateRuntimeUpdater",
]
