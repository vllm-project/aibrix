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

import uuid

from aibrix.batch.storage.generic_storage import LocalDiskFiles, StorageType

# [TODO] Add S3 as another storage
from aibrix.batch.storage.tos_storage import TOSStorage

current_job_offsets = {}
job_input_requests = {}
p_storage = None
NUM_REQUESTS_PER_READ = 1024


def initialize_batch_storage(storage_type=StorageType.LocalDiskFile, params={}):
    """Initialize storage type. Now it support files and TOS.

    For some storage type, user needs to pass in other parameters to params.
    """
    global p_storage
    if storage_type == StorageType.LocalDiskFile:
        p_storage = LocalDiskFiles()
    elif storage_type == StorageType.TOS:
        p_storage = TOSStorage(**params)
    else:
        raise ValueError("Unknown storage type")


def upload_input_data(inputDataFileName):
    """Upload job input data file to storage.

    Args:
        inputDataFileName (str): an input file string.
    """
    job_id = uuid.uuid1()
    p_storage.write_job_input_data(job_id, inputDataFileName)

    current_job_offsets[job_id] = 0
    return job_id


def read_job_requests(job_id, start_index, num_requests):
    """Read job requests starting at index: start_index.

    Instead of reading from storage per request, this maintains a list of requests
    in memory with a length of NUM_REQUESTS_PER_READ.
    It also supports random access, if backward read is necessary.
    """

    if job_id not in current_job_offsets:
        print(f"Create job {job_id} first. Can not find corresponding job ID!!")
        return []

    # if no request is cached, this reads a list of requests from storage.
    if job_id not in job_input_requests:
        request_inputs = p_storage.read_job_input_data(job_id, 0, NUM_REQUESTS_PER_READ)
        job_input_requests[job_id] = request_inputs

    current_start_idx = current_job_offsets[job_id]
    current_end_idx = current_start_idx + len(job_input_requests[job_id])

    # this reads request backward, so it only reads necessary part.
    if start_index < current_start_idx:
        if start_index + num_requests < current_start_idx + 1:
            temp_len = num_requests
            diff_requests = p_storage.read_job_input_data(job_id, start_index, temp_len)
            job_input_requests[job_id] = diff_requests
        else:
            temp_len = current_start_idx + 1 - start_index
            diff_requests = p_storage.read_job_input_data(job_id, start_index, temp_len)
            job_input_requests[job_id] = diff_requests + job_input_requests[job_id]

        current_job_offsets[job_id], current_start_idx = start_index, start_index
        current_end_idx = current_start_idx + len(job_input_requests[job_id])

    # the cached parts miss already, this throws away old caches.
    if start_index >= current_end_idx:
        current_job_offsets[job_id] = start_index
        job_input_requests[job_id] = []
        current_start_idx, current_end_idx = start_index, start_index

    # now this reads necessary requests at least for a length of num_requests.
    if start_index + num_requests > current_end_idx:
        temp_len = start_index + num_requests - current_end_idx
        diff_requests = p_storage.read_job_input_data(job_id, current_end_idx, temp_len)
        job_input_requests[job_id] = job_input_requests[job_id] + diff_requests
        current_end_idx = current_job_offsets[job_id] + len(job_input_requests[job_id])

    available_num_req = min(num_requests, current_end_idx - start_index)
    start_offset = start_index - current_start_idx

    requests = job_input_requests[job_id][
        start_offset : start_offset + available_num_req
    ]
    # print("debug", len(requests))
    return requests


def put_storage_job_results(job_id, start_index, requests_results):
    """Write job results on a specific index."""
    p_storage.write_job_output_data(job_id, start_index, requests_results)


def get_storage_job_results(job_id, start_index, num_requests):
    """Read job requests results."""
    return p_storage.read_job_output_data(job_id, start_index, num_requests)


def remove_storage_job_data(job_id):
    """Remove job all relevant data."""
    p_storage.delete_job_data(job_id)


def get_job_request_len(job_id):
    """Get the number of requests for the job_id."""
    return p_storage.get_job_number_requests(job_id)
