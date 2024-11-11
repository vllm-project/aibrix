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
import os
from abc import ABC, abstractmethod
from enum import Enum

LOCAL_STORAGE_PATH_VAR = "LOCAL_STORAGE_PATH"


class StorageType(Enum):
    LocalDiskFile = 1
    S3 = 2
    TOS = 3


class PersistentStorage(ABC):
    """
    This is an abstract class.

    A storage should implement this class, such as Local files, TOS and S3.
    Any storage implementation are transparent to external components.
    """

    @abstractmethod
    def write_job_input_data(self, job_id, inputDataFileName):
        pass

    @abstractmethod
    def read_job_input_data(self, job_id, start_index=0, num_requests=-1):
        pass

    @abstractmethod
    def write_job_output_data(self, job_id, output_list):
        pass

    @abstractmethod
    def delete_job_data(self, job_id):
        pass

    def get_current_storage_type(self):
        print("Current storage type: ", self._storage_type_name)
        return self._storage_type_name

    @classmethod
    def create_storage(cls, storage_type=StorageType.LocalDiskFile):
        if storage_type == StorageType.LocalDiskFile:
            return LocalDiskFiles()
        else:
            raise ValueError("Unknown storage type")


class LocalDiskFiles(PersistentStorage):
    """
    This stores all job data in local disk as files.
    """

    def __init__(self):
        print("Setup ENV VAR for local storage path! ")

        if LOCAL_STORAGE_PATH_VAR in os.environ:
            self.directory_path = os.environ[LOCAL_STORAGE_PATH_VAR]
        else:
            self.directory_path = os.path.abspath(os.path.dirname(__file__))

        self.directory_path = self.directory_path + "/data/"
        os.makedirs(self.directory_path, exist_ok=True)
        print("Storage path is located: ", self.directory_path)

    def write_job_input_data(self, job_id, inputDataFileName):
        """This writes requests file to local disk."""
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
        print(f"Storage side receives {num_valid_request} request.")

        directory_path = self.directory_path + str(job_id) + "/"
        os.makedirs(directory_path)

        inputFileName = "input.json"
        inputJsonName = directory_path + inputFileName
        with open(inputJsonName, "w") as file:
            for obj in request_list:
                file.write(json.dumps(obj) + "\n")

    def read_job_input_data(self, job_id, start_index=0, num_requests=-1):
        """Read job requests input from local disk."""
        directory_path = self.directory_path + str(job_id) + "/"
        inputFileName = "input.json"
        inputJsonName = directory_path + inputFileName

        request_inputs = []
        if not os.path.exists(inputJsonName):
            print(f"job {job_id} does not exist in storage!")
            return request_inputs

        with open(inputJsonName, "r") as file:
            for _ in range(start_index):
                next(file)
                if not file:
                    print("read requests is out of index, not enough size.")
                    return request_inputs

            if num_requests > 0:
                for _ in range(num_requests):
                    line = file.readline()
                    if not line:  # End of file reached
                        break
                    data = json.loads(line)
                    request_inputs.append(data)
            else:
                # Parse JSON data into a Python dictionary
                for line in file.readlines():
                    if len(line) <= 1:
                        continue
                    data = json.loads(line)
                    request_inputs.append(data)
        # print("debug: ", len(request_inputs))
        return request_inputs

    def get_job_number_requests(self, job_id):
        """Get job requests length from local disk."""
        directory_path = self.directory_path + str(job_id) + "/"
        inputFileName = "input.json"
        inputJsonName = directory_path + inputFileName

        if not os.path.exists(inputJsonName):
            print(f"job {job_id} does not exist in storage!")
            return 0

        with open(inputJsonName, "r") as file:
            return sum(1 for line in file)

        return 0

    def write_job_output_data(self, job_id, start_index, output_list):
        """Write job results output as files."""
        directory_path = self.directory_path + str(job_id) + "/"

        if not os.path.exists(directory_path):
            print(
                f"Error: Job {job_id} does not exist, perhaps need to create Job first!"
            )
            return
        output_file_path = directory_path + "output.json"

        with open(output_file_path, "a+") as file:
            for _ in range(start_index):
                next(file, None)
                if not file:
                    print("writing requests is out of index.")
                    return

            for obj in output_list:
                file.write(json.dumps(obj) + "\n")
            file.truncate()

    def read_job_output_data(self, job_id, start_index, num_requests):
        """Read job results output from local disk as files."""
        directory_path = self.directory_path + str(job_id) + "/"

        output_data = []
        if not os.path.exists(directory_path):
            print(
                f"Error: Job {job_id} does not exist, perhaps need to create Job first!"
            )
            return output_data
        output_file_path = directory_path + "output.json"

        with open(output_file_path, "r") as file:
            for _ in range(start_index):
                next(file)
                if not file:
                    print("reading requests output is out of index.")
                    return output_data

            num_lines = 0
            for line in file.readlines():
                if len(line) <= 1:
                    continue
                data = json.loads(line)
                output_data.append(data)
                num_lines += 1
                if num_lines == num_requests:
                    break

        return output_data

    def delete_job_data(self, job_id):
        """Delete all input and output files for the job."""
        directory_path = self.directory_path + str(job_id) + "/"

        input_file_path = directory_path + "input.json"
        try:
            os.remove(input_file_path)
        except FileNotFoundError:
            print(f"Job ID {input_file_path} does not exist.")
        except Exception as e:
            print(f"Error: {e}")

        output_file_path = directory_path + "output.json"
        if os.path.exists(output_file_path):
            try:
                os.remove(output_file_path)
            except FileNotFoundError:
                print(f"Job output {output_file_path} does not exist.")
            except Exception as e:
                print(f"Error: {e}")

        try:
            os.rmdir(directory_path)
        except FileNotFoundError:
            print(f"Job Directory {directory_path} does not exist.")
        except OSError as e:
            print(f"Error: {e} - Job Directory is not empty or can't be deleted.")
