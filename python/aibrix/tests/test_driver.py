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

import time
import json
import asyncio
from aibrix.batch.driver import BatchDriver
from aibrix.batch.constant import EXPIRE_INTERVAL

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

    await asyncio.sleep(11 * EXPIRE_INTERVAL)

    print("start to read results")
    results = _driver.retrieve_job_result(job_id)
    for i, req_result in enumerate(results):
        print(i, req_result)


if __name__ == "__main__":
    asyncio.run(driver_proc())