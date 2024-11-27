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
from typing import List, Protocol

from aibrix.gpu_optimizer.optimizer import GPUProfile

logger = logging.getLogger("aibrix.gpuoptimizer.profile_reader")


class ProfileReader(Protocol):
    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""


class FileProfileReader:
    def __init__(self, filepath: str) -> None:
        self.filepath = filepath

    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""
        with open(self.filepath, "r") as f:
            try:
                # Try parse as singal json
                profiles = json.load(f)
            except Exception:
                try:
                    # Try parse as list of json (jsonl)
                    profiles = []
                    for line in f:
                        if line.strip() == "":
                            continue
                        profiles.append(json.loads(line))
                except Exception as e:
                    logger.warning(
                        f"Invalid profile file format, expected list or dict: {e}"
                    )

        if isinstance(profiles, dict):
            profiles = [profiles]
        elif not isinstance(profiles, list):
            logger.warning("Invalid profile file format, expected list or dict.")

        return [GPUProfile(**profile) for profile in profiles]


class RedisProfileReader:
    def __init__(
        self, redis_client, model_name: str, key_prefix: str = "aibrix:profile_%s_"
    ) -> None:
        self.client = redis_client
        self.key_prefix = key_prefix % (model_name)

    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""
        cursor = 0
        matching_keys = []
        while True:
            cursor, keys = self.client.scan(cursor=cursor, match=f"{self.key_prefix}*")
            for key in keys:
                matching_keys.append(key)
            if cursor == 0:
                break
        if len(matching_keys) == 0:
            logger.warning(f"No profiles matching {self.key_prefix}* found in Redis")

        # Retrieve the objects associated with the keys
        records = []
        for key in matching_keys:
            # Deserialize by json: dict[string]int
            profile_data = self.client.get(key)
            if profile_data is None:
                raise Exception(f"Failed to retrieve {key.decode()} from Redis.")

            # Deserialize by json: dict[string]int
            profile = json.loads(profile_data)
            if not isinstance(profile, dict):
                raise Exception("Profile is not a dictionary")

            records.append(GPUProfile(**profile))

        return records
