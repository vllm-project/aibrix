import json
import os

import aibrix.batchAPI.storage as _storage


def generate_input_data(num_requests):
    input_name = "./sample_job_input.json"
    data = None
    with open(input_name, 'r') as file:
        for line in file.readlines():
            data = json.loads(line)
            break

    # In the following tests, we use this custom_id
    # to check if the read and write are exactly the same.
    with open("one_job_input.json", 'w') as file:
        for i in range(num_requests):
            data["custom_id"] = i
            file.write(json.dumps(data) + '\n')

def test_submit_job_input():
    num_request = 100
    generate_input_data(num_request)
    job_id = _storage.submitJobInput("./one_job_input.json")
    print("succesfully create job: ", job_id)

    input_num = _storage.getJobNumRequest(job_id)
    assert( input_num == num_request)
    print("Total # requeust: ", num_request)

    _storage.deleteJob(job_id)
    print("remove job id:", job_id)
    os.remove("./one_job_input.json")


def test_read_job_input():

    num_request = 100
    generate_input_data(num_request)
    job_id = _storage.submitJobInput("./one_job_input.json")
    print("succesfully create job: ", job_id)

    # First round, start with an arbitrary index.
    start_idx, num = 50, 10
    requests = _storage.getJobInputRequests(job_id, start_idx, num)
    for i, req in enumerate(requests):
        custom_id = req["custom_id"]
        assert(custom_id == start_idx+i)

    # Second round, this is a follow-up read.
    start_idx, num = 60, 10
    requests = _storage.getJobInputRequests(job_id, start_idx, num)
    for i, req in enumerate(requests):
        custom_id = req["custom_id"]
        assert(custom_id == start_idx+i)

    # Thrid round, it reads backward. 
    start_idx, num = 30, 20
    requests = _storage.getJobInputRequests(job_id, start_idx, num)
    for i, req in enumerate(requests):
        custom_id = req["custom_id"]
        assert(custom_id == start_idx+i)

    _storage.deleteJob(job_id)
    print("remove job id:", job_id)
    os.remove("./one_job_input.json")


def test_job_output():
    num_request = 100
    generate_input_data(num_request)
    job_id = _storage.submitJobInput("./one_job_input.json")
    print("succesfully create job: ", job_id)

    start_idx, num = 50, 10
    requests = _storage.getJobInputRequests(job_id, start_idx, num)
    
    # Now assuming the output are the same as the input.
    _storage.putJobResults(job_id, 0, requests)
    output_reqs = _storage.getJobResults(job_id, 0, len(requests))
    for i, req in enumerate(output_reqs):
        custom_id = req["custom_id"]
        assert(custom_id == start_idx+i)
    
    _storage.deleteJob(job_id)
    print("remove job id:", job_id)
    os.remove("./one_job_input.json")

