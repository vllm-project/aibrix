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

import bytedtos
from io import StringIO
import json
from aibrix.batch.storage.generic_storage import PersistentStorage

class TOSStorage(PersistentStorage):

    def __init__(self):
        """
        This initializes client configuration to a
        TOS online bucket. 
        All job information will be submitted to this 
        TOS bucket.
        Now this TOS bucket is under Xin's account.
        TOS objects can also be checked through this link.
        https://cloud.bytedance.net/tos/bucket/5788480/objects?x-resource-account=public&x-bc-vregion=China-North&region=default
        
        [TODO]Create a public account for TOS bucket.
        """

        self._client = None
        ak= "S7OZD2516J8MV1A8I5Y0"
        sk = "amd9r6XxUB6alC79/LtfPxMdtbbIVMcFXK2ashCV"
        bucket_name = "aibrix-batch"
        sub_domain = "tos-cn-north.byted.org"
        client_psm = "toutiao.tos.tosapi"
        is_stream = False

        try: 
            self._client = bytedtos.Client(bucket_name, ak,
                        # The endpoint parameter is optional, which is used to specify whether to perform initialization based on the endpoint.
                        endpoint=sub_domain,
                        # The stream parameter is optional, which is used to specify whether to perform the streaming download.
                        stream=is_stream,
                        # The remote_psm parameter is optional, which is used to specify the PSM of the client.
                        remote_psm=client_psm,
                        # The timeout parameter is optional, which is used to specify the request timeout period.
                        timeout=60,
                        # The connection_time parameter is optional, which is used to specify the connection timeout period.
                        connect_timeout=60,
                        # Use connection_pool_size =10 to set the size of the connection pool to 10.
                        connection_pool_size=10)    

            obj_key = "test_key"
            string_io_obj = StringIO("hello world")
            self._client.put_object(obj_key, string_io_obj)
            resp = self._client.get_object(obj_key)
            print("read data:", resp.data)
            print("TOS client is created. Action succ. code: {}, request_id: {}".format(resp.status_code, resp.headers[bytedtos.consts.ReqIdHeader]))
        except bytedtos.TosException as e: 
            print("Attempting to create TOS client failed. code: {}, request_id: {}, message: {}".format(e.code, e.request_id, e.msg))
            
    def write_job_input_data(self, job_id, inputDataFileName):
        """
        Each request is an object. 
        the format is jobID_input_requestId
        """
        request_list = []
        # Open the JSON file
        with open(inputDataFileName, "r") as file:
            # Parse JSON data into a Python dictionary
            for line in file.readlines():
                if len(line) <= 1:
                    continue
                data = json.loads(line)
                request_list.append(data)

        num_valid_request = len(request_list)
        object_prefix = f"{job_id}/input"
        for i, req_data in enumerate(request_list):
            obj_key = f"{object_prefix}/{i}"
            json_str = json.dumps(req_data)
            string_io_obj = StringIO(json_str)
            self._client.put_object(obj_key, string_io_obj)
        
        print(f"TOS receives a job with {num_valid_request} request.")
        

    def read_job_input_data(self, job_id, start_index=0, num_requests=-1):
        """ Read job input request data """

        request_inputs = []
        object_prefix = f"{job_id}/input"
        for i in range(num_requests):
            idx = start_index + i
            obj_key = f"{object_prefix}/{idx}"
            resp = self._client.get_object(obj_key)
            json_str = resp.data.decode('utf-8')
            json_obj = json.loads(json_str)
            request_inputs.append(json_obj)

        return request_inputs
    
    def write_job_output_data(self, job_id, start_index, output_list):
        """ 
        Write job results to TOS bucket.
        The key of job request output is in the format of 
        jobID_output_requestId.
        """

        object_prefix = f"{job_id}/output"
        for i, req_data in enumerate(output_list):
            idx = start_index + i
            obj_key = f"{object_prefix}/{idx}"
            json_str = json.dumps(req_data)
            string_io_obj = StringIO(json_str)
            self._client.put_object(obj_key, string_io_obj)
        
        num_valid_request = len(output_list)
        print(f"Write to TOS for job {job_id} with {num_valid_request} request.")
        
    def read_job_output_data(self, job_id, start_index, num_requests):
        """Read job results output from TOS bucket."""
        
        request_results = []
        object_prefix = f"{job_id}/output"
        for i in range(num_requests):
            idx = start_index + i
            obj_key = f"{object_prefix}/{idx}"
            resp = self._client.get_object(obj_key)
            json_str = resp.data.decode('utf-8')
            json_obj = json.loads(json_str)
            request_results.append(json_obj)

        return request_results

    def delete_job_data(self, job_id):
        pass

    def get_job_number_requests(self, job_id):
        """
        [TODO] xin
        Need to use TOS list_prefix to count the number of reqeust"
        """
        pass