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


from .batch_storage import (get_job_request_len, get_job_results,
                            put_job_results, read_job_requests, removeJobData,
                            uploadInputData)


def submitJobInput(inputDataFile):
    """ Upload job input data file to storage.

    Args:
        inputDataFile (str): an input file string. Each line is a request.
    """
    job_id = uploadInputData(inputDataFile)
    return job_id
        
def deleteJob(job_id):
    """ Delete job given job ID. This removes all data associated this job, 
        including input data and output results.
    """
    removeJobData(job_id)

def getJobInputRequests(job_id, start_index, num_requests):
    """ Read job input requests specified job Id.

    Args:
        job_id : job_id is returned by job submission.
        start_index : request starting index for read.
        num_requests: total number of requests needed to read.
    """
    return read_job_requests(job_id, start_index, num_requests)

def getJobNumRequest(job_id):
    """ Get the number of valid requests for this submitted job. 
    """
    return get_job_request_len(job_id)

def putJobResults(job_id, start_index, requests_results):
    """ Write job requests results to storage. 

    Args:
        job_id : job_id is returned by job submission.
        start_index : requests index to write.
        requests_results: a list of json objects as request results to write.
    """
    put_job_results(job_id, start_index, requests_results)

def getJobResults(job_id, start_index, num_requests):
    """ Read job requests results from storage. 

    Args:
        job_id : job_id is returned by job submission.
        start_index : requests index to read.
        num_requests: total number of requests output needed to read.
    """
    return get_job_results(job_id, start_index, num_requests)
