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

import json
import logging
from io import StringIO

import tos

from aibrix.batch.storage.generic_storage import PersistentStorage


class TOSStorage(PersistentStorage):
    def __init__(self, access_key, secret_key, endpoint, region, bucket_name):
        """
        This initializes client configuration to a
        TOS online bucket. All job information will be submitted to this
        TOS bucket. The two input keys are from Volcano TOS account.
        """

        self._client = None
        self._bucket_name = bucket_name
        ak, sk = access_key, secret_key

        try:
            self._client = tos.TosClientV2(ak, sk, endpoint, region)
            logging.info("Finished creating TOS client!!!")
        except tos.exceptions.TosClientError as e:
            logging.error(
                f"fail with client error, message:{e.message}, cause: {e.cause}"
            )
        except tos.exceptions.TosServerError as e:
            logging.error(f"fail with server error, code: {e.code}")
            logging.error(f"error with request id: {e.request_id}")
            logging.error(f"error with message: {e.message}")
            logging.error(f"error with http code: {e.status_code}")
            logging.error(f"error with ec: {e.ec}".format())
            logging.error(f"error with request url: {e.request_url}")
        except Exception as e:
            logging.error(f"fail with unknown error: {e}")
            logging.error("Attempting to create TOS client failed.")

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
        try:
            for i, req_data in enumerate(request_list):
                obj_key = f"{object_prefix}/{i}"
                json_str = json.dumps(req_data)
                string_io_obj = StringIO(json_str)
                self._client.put_object(
                    self._bucket_name, obj_key, content=string_io_obj
                )
        except Exception as e:
            logging.error(f"TOS write fails with unknown error: {e}")

        logging.info(f"TOS receives a job with {num_valid_request} request.")

    def read_job_input_data(self, job_id, start_index=0, num_requests=-1):
        """Read job input request data"""

        request_inputs = []
        object_prefix = f"{job_id}/input"
        try:
            for i in range(num_requests):
                idx = start_index + i
                obj_key = f"{object_prefix}/{idx}"
                object_stream = self._client.get_object(self._bucket_name, obj_key)
                json_obj = json.loads(object_stream.read())
                request_inputs.append(json_obj)
        except Exception as e:
            logging.error(f"TOS reading job input fails with unknown error: {e}")
        return request_inputs

    def write_job_output_data(self, job_id, start_index, output_list):
        """
        Write job results to TOS bucket.
        The key of job request output is in the format of
        jobID_output_requestId.
        """

        object_prefix = f"{job_id}/output"
        try:
            for i, req_data in enumerate(output_list):
                idx = start_index + i
                obj_key = f"{object_prefix}/{idx}"
                json_str = json.dumps(req_data)
                string_io_obj = StringIO(json_str)
                self._client.put_object(
                    self._bucket_name, obj_key, content=string_io_obj
                )
        except Exception as e:
            logging.error("TOS writing output fails with unknown error: {}".format(e))
        num_valid_request = len(output_list)
        logging.info(f"Write to TOS for job {job_id} with {num_valid_request} request.")

    def read_job_output_data(self, job_id, start_index, num_requests):
        """Read job results output from TOS bucket."""

        request_results = []
        object_prefix = f"{job_id}/output"

        try:
            for i in range(num_requests):
                idx = start_index + i
                obj_key = f"{object_prefix}/{idx}"
                object_stream = self._client.get_object(self._bucket_name, obj_key)
                json_obj = json.loads(object_stream.read())
                request_results.append(json_obj)
        except Exception as e:
            logging.error(f"TOS reading request output fails with unknown error: {e}")
        return request_results

    def delete_job_data(self, job_id):
        """This deletes all request data for the given job ID,
        including both input data and output data.
        """
        object_prefix = f"{job_id}/"
        is_truncated = True
        marker = ""
        try:
            while is_truncated:
                out = self._client.list_objects(
                    self._bucket_name, prefix=object_prefix, marker=marker
                )
                for obj in out.contents:
                    self._client.delete_object(self._bucket_name, obj.key)
                is_truncated = out.is_truncated
                marker = out.next_marker
        except Exception as e:
            logging.error(f"Deleting job fails with unknown error: {e}")

    def get_job_number_requests(self, job_id):
        """
        This read the number of reqeust by listing the number of input requests.
        """
        object_prefix = f"{job_id}/input/"
        num_requests = 0
        try:
            is_truncated = True
            next_continuation_token = ""

            while is_truncated:
                out = self._client.list_objects_type2(
                    self._bucket_name,
                    delimiter="/",
                    prefix=object_prefix,
                    continuation_token=next_continuation_token,
                )
                is_truncated = out.is_truncated
                next_continuation_token = out.next_continuation_token
                # This ignore directory in common_prefixes
                num_requests += len(out.contents)
        except Exception as e:
            logging.error(f"Listing number of reqeusts fails with unknown error: {e}")

        return num_requests
