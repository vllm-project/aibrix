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

import tempfile
from io import BytesIO
from pathlib import Path
from urllib.parse import urlparse

import numpy as np
from boto3.s3.transfer import TransferConfig

from aibrix.loader.utils import (
    _create_s3_client,
    _create_tos_client,
    _parse_bucket_info_from_uri,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)


class LoadFile:
    def __init__(self, file_source: str) -> None:
        self.file_source = file_source

    def load_whole_file(self, num_threads: int = 1):
        raise NotImplementedError

    def load_to_bytes(self, offset: int, count: int):
        raise NotImplementedError


class LocalFile(LoadFile):
    def __init__(self, file: str) -> None:
        if not Path(file).exists():
            raise ValueError(f"file {file} not exist")

        self.file = file
        super().__init__(file_source="local")

    def load_whole_file(self, num_threads: int = 1):
        if num_threads != 1:
            logger.warning(
                f"num_threads {num_threads} is not supported for local file."
            )

        tensor_bytes = np.memmap(
            self.file,
            dtype=np.uint8,
            mode="c",
        )
        return tensor_bytes.tobytes()

    def load_to_bytes(self, offset: int, count: int):
        arr = np.fromfile(self.file, dtype=np.uint8, offset=offset, count=count)
        return arr.tobytes()


class S3File(LoadFile):
    def __init__(self, file: str) -> None:
        self.file = file
        bucket_name, bucket_path = _parse_bucket_info_from_uri(file, "s3")
        self.bucket_name = bucket_name
        self.bucket_path = bucket_path
        try:
            s3_client = _create_s3_client()
            s3_client.head_object(Bucket=bucket_name, Key=bucket_path)
        except Exception as e:
            raise ValueError(f"S3 bucket path {bucket_path} not exist for {e}.")
        super().__init__(file_source="s3")

    def load_whole_file(self, num_threads: int):
        s3_client = _create_s3_client()

        config_kwargs = {
            "max_concurrency": num_threads,
            "use_threads": True,
        }
        config = TransferConfig(**config_kwargs)

        data = BytesIO()
        s3_client.download_fileobj(
            Bucket=self.bucket_name,
            Key=self.bucket_path,
            Fileobj=data,
            Config=config,
        )
        return data.getbuffer()

    def load_whole_file_v2(self, num_threads: int = 1):
        _file_name = self.bucket_path.split("/")[-1]
        local_path = Path("/tmp/aibrix/loader/s3/")
        local_path.mkdir(parents=True, exist_ok=True)
        local_file = local_path.joinpath(_file_name).absolute()

        s3_client = _create_s3_client()

        config_kwargs = {
            "max_concurrency": num_threads,
            "use_threads": True,
        }
        config = TransferConfig(**config_kwargs)

        s3_client.download_file(
            Bucket=self.bucket_name,
            Key=self.bucket_path,
            Filename=str(
                local_file
            ),  # S3 client does not support Path, convert it to str
            Config=config,
        )

        tensor_bytes = np.memmap(
            local_file,
            dtype=np.uint8,
            mode="c",
        )
        return tensor_bytes.tobytes()

    def load_to_bytes(self, offset: int, count: int):
        s3_client = _create_s3_client()

        range_header = f"bytes={offset}-{offset+count-1}"
        resp = s3_client.get_object(
            Bucket=self.bucket_name, Key=self.bucket_path, Range=range_header
        )
        return resp.get("Body").read()

class TOSFile(LoadFile):
    def __init__(self, file: str) -> None:
        self.file = file
        bucket_name, bucket_path = _parse_bucket_info_from_uri(file, "tos")
        self.bucket_name = bucket_name
        self.bucket_path = bucket_path
        try:
            tos_client = _create_tos_client()
            tos_client.head_object(bucket=bucket_name, key=bucket_path)
        except Exception as e:
            raise ValueError(f"S3 bucket path {bucket_path} not exist for {e}.")
        super().__init__(file_source="tos")

    def load_whole_file(self, num_threads: int = 1):
        _file_name = self.bucket_path.split("/")[-1]

        with tempfile.TemporaryDirectory() as local_path:
            local_file = Path(local_path).joinpath(_file_name).absolute()

            tos_client = _create_tos_client()
            tos_client.download_file(
                bucket=self.bucket_name,
                key=self.bucket_path,
                file_path=str(
                    local_file
                ),  # TOS client does not support Path, convert it to str
                task_num=num_threads,
            )

            tensor_bytes = np.memmap(
                local_file,
                dtype=np.uint8,
                mode="c",
            )
        return tensor_bytes.tobytes()

    def load_to_bytes(self, offset: int, count: int):
        tos_client = _create_tos_client()

        range_header = f"bytes={offset}-{offset+count-1}"
        resp = tos_client.get_object(
            bucket=self.bucket_name, 
            key=self.bucket_path,
            range=range_header
        )
        return resp.read()



if __name__ == "__main__":
    file = "tos://aibrix-artifact-testing/models/HuggingFaceTB/SmolLM-1.7B/model-00001-of-00002.safetensors"
    s3_file = TOSFile(file)

    length_bytes = s3_file.load_to_bytes(offset=0, count=8)
    import json
    import struct
    length_of_header = struct.unpack("<Q", length_bytes)[0]

    meta_bytes = s3_file.load_to_bytes(offset=8, count=length_of_header)
    meta_dict = json.loads(meta_bytes.decode("utf-8"))
    print(meta_dict)
