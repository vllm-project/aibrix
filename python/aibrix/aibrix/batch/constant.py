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


# The following are all constants.
import os

# This is the time interval for the sliding window to check.
EXPIRE_INTERVAL: float = 1

# This is the job pool size in job scheduler.
# It should be proportional to resource size in the backend.
# Can be configured via AIBRIX_BATCH_JOB_POOL_SIZE environment variable.
DEFAULT_JOB_POOL_SIZE = int(os.environ.get("AIBRIX_BATCH_JOB_POOL_SIZE", "1"))

# Validate job pool size
if not (1 <= DEFAULT_JOB_POOL_SIZE <= 100):
    raise ValueError(
        f"AIBRIX_BATCH_JOB_POOL_SIZE must be between 1 and 100, got {DEFAULT_JOB_POOL_SIZE}"
    )

# Job opts are for testing purpose.
BATCH_OPTS_FAIL_AFTER_N_REQUESTS = "fail_after_n_requests"
