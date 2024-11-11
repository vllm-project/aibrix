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
import json

from aibrix.batch.constant import EXPIRE_INTERVAL
from aibrix.batch.driver import BatchDriver
from aibrix.batch.job_manager import JobStatus


def generate_input_data(num_requests, local_file):
    input_name = "./sample_job_input.json"
    data = None
    with open(input_name, "r") as file:
        for line in file.readlines():
            data = json.loads(line)
            break

    # In the following tests, we use this custom_id
    # to check if the read and write are exactly the same.
    with open(local_file, "w") as file:
        for i in range(num_requests):
            data["custom_id"] = i
            file.write(json.dumps(data) + "\n")


async def driver_proc():
    """
    This is main driver process on how to submit jobs.
    """
    _driver = BatchDriver()

    num_request = 10
    local_file = "./one_job_input.json"
    generate_input_data(num_request, local_file)
    job_id = _driver.upload_batch_data("./one_job_input.json")

    _driver.create_job(job_id, "sample_endpoint", "20m")
    status = _driver.get_job_status(job_id)
    assert status == JobStatus.PENDING
    print("Current status: ", status)

    await asyncio.sleep(5 * EXPIRE_INTERVAL)
    status = _driver.get_job_status(job_id)
    print("Current status: ", status)
    assert status == JobStatus.IN_PROGRESS

    await asyncio.sleep(6 * EXPIRE_INTERVAL)
    status = _driver.get_job_status(job_id)
    print("Current status: ", status)
    assert status == JobStatus.COMPLETED

    print("Retrieve results")
    results = _driver.retrieve_job_result(job_id)
    for i, req_result in enumerate(results):
        print(i, req_result)

    _driver.clear_job(job_id)
    print(f"Job {job_id} is cleaned.")
    await asyncio.sleep(2 * EXPIRE_INTERVAL)


if __name__ == "__main__":
    asyncio.run(driver_proc())
