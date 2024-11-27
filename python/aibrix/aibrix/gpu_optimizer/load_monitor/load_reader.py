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
import re
from datetime import datetime
from typing import Any, List, Optional, Protocol, Union

import numpy as np
import pandas as pd
from redis import Redis

logger = logging.getLogger("aibrix.gpu_optimizer.load_reader")

unittest_filepath = "unittest_694cb6cf-f5b3-42ca-b3c1-55ff0b358bdb"


class LoadRecord(tuple):
    """LoadRecord models a tuple with the following fields: ts, input tokens, output tokens, and frequency."""

    def __new__(cls, *args: Any, **kwargs: Any) -> "LoadRecord":
        return super(LoadRecord, cls).__new__(cls, args)

    @property
    def ts(self) -> float:
        return self[0]

    @property
    def input_tokens(self) -> float:
        return self[1]

    @property
    def output_tokens(self) -> float:
        return self[2]

    @property
    def freq(self) -> int:
        if len(self) < 4:
            return 1
        return self[3]


class LoadReader(Protocol):
    def read(self, ts: float = 0.0) -> List[LoadRecord]:
        """Read the next batch of records from the data source."""

    def progress(self) -> str:
        """Return the progress description of the data source."""

    def next_available(self) -> float:
        """Return the timestamp next batch of data will be available."""


class DatasetLoadReader:
    """DatasetLoadReader reads the load records from a dataset.
    To match the behavior of the gateway, the input and output tokens are rounded to the nearest integer of log2.
    """

    def __init__(
        self, filepath, rps: int = 10, scale: float = 1.0, interval: int = 10
    ) -> None:
        if filepath != unittest_filepath:
            self.df = pd.read_csv(filepath)
            self.df["input_tokens"] = self.log2_aggregate(
                self.df["input_tokens"] * scale, 1
            )
            self.df["output_tokens"] = self.log2_aggregate(
                self.df["output_tokens"] * scale, 1
            )
            # self.df['input_tokens'] = self.stair_aggregate(self.df['input_tokens'] * scale)
            # self.df['output_tokens'] = self.stair_aggregate(self.df['output_tokens'] * scale)

        self.rps = rps
        self.interval = 10
        self.n_read = 0
        self.n = 0

    def log2_aggregate(self, series: pd.Series, precision: int = 0) -> List:
        return np.round(np.log2(series), precision)

    def stair_aggregate(self, series: List, skip_log2: bool = False) -> List:
        BaseBucketBits = 3
        ScalingBits = 4

        scale = (
            np.maximum(np.floor(np.log2(series)) - BaseBucketBits, 0) // ScalingBits + 1
        )
        bucketbits = np.maximum(
            (scale - 1) * ScalingBits + BaseBucketBits - 1, BaseBucketBits
        )
        aggregated = np.maximum(series - np.mod(series, 2**bucketbits), 1)
        return aggregated if skip_log2 else np.log2(aggregated)

    def read(self, ts: float = 0.0) -> List[LoadRecord]:
        """Read the next batch of records from the data source.

        args:
            ts: float, ignored.
        """
        records = []
        # Simulate the arrival of requests using Poisson distribution
        n_batch = np.random.poisson(self.rps * self.interval)
        self.last_ts = ts
        end = self.n_read + n_batch
        if end > len(self.df):
            end = len(self.df)

        chunk = self.df.iloc[self.n_read : end]
        self.n_read = end
        for _, row in chunk.iterrows():
            records.append(
                LoadRecord(
                    self.n * self.interval, row["input_tokens"], row["output_tokens"]
                )
            )
        self.n += 1

        return records

    def progress(self) -> str:
        return f"{round(self.n_read / len(self.df) * 100, 2)}%"

    def next_available(self) -> float:
        """Dataset is available to read anytime."""
        return datetime.now().timestamp()


class GatewayLoadReader:
    """GatewayLoadReader reads the load records from gateway generated statistics stored in Redis.
    Currently, gateway will aggregate the load records into a single key per interval(e.g., 10s) with the following format:

        aibrix:{model_name}_request_trace_{ts}

    The value of the key is a json object with the following format:

    {
        "{round(log2(input_tokens))}-{round(log2(output_tokens))}: {frequency}
    }
    """

    def __init__(
        self, redis_client: Redis, model_name: str, key_ts_alignment: int = 10
    ) -> None:
        self.client: Redis = redis_client
        self.start = 0.0
        self.last_ts = 0.0
        self.prefix = f"aibrix:{model_name}_request_trace_"
        self.key_ts_alignment = key_ts_alignment

    def read(self, ts: float = 0.0) -> List[LoadRecord]:
        """Read the next batch of records from the data source."""
        try:
            if self.start == 0:
                self.start = ts
                return self.read_first()

            # Align the ts according to key_ts_alignment
            ts = ts - ts % self.key_ts_alignment
            if ts <= self.last_ts:
                # Seen
                return []

            # TODO: Now profile seems to be have a interval delay. Further investigation is needed.
            profiles = self.read_key(
                f"{self.prefix}{int(ts - self.key_ts_alignment)}", True
            )
            self.last_ts = ts

            if profiles is None or len(profiles) == 0:
                return []

            return self._parse_profiles(profiles, ts)

        except Exception as e:
            logger.warning(f"Failed to read from Redis: {e}")
            return []

    def read_first(self) -> List[LoadRecord]:
        """Read the first batch of records from the data source."""
        cursor = 0
        matching_keys = []
        while True:
            cursor, keys = self.client.scan(cursor=cursor, match=f"{self.prefix}*")  # type: ignore
            for key in keys:
                # Decode the key from bytes to string
                strkey = key.decode()
                match = re.search(r"(?:.*?)_(\d+)$", strkey)
                if match is None:
                    logger.warning(f"Unexpected {strkey} from Redis")
                    continue
                matching_keys.append((key, int(match.group(1))))
            if cursor == 0:
                break
        if len(matching_keys) == 0:
            self.last_ts = datetime.now().timestamp()
            logger.info(
                f"No pre-existed load profile matching {self.prefix}* found in Redis"
            )
            return []

        # Sort by ts to ensure profiles are processed by time order.
        matching_keys = sorted(matching_keys, key=lambda k: k[1])

        # Retrieve the objects associated with the keys
        records: List[LoadRecord] = []
        for key in matching_keys:
            try:
                # Deserialize by json: dict[string]int
                self.last_ts = key[1]
                profiles = self.read_key(key[0], False)
                if profiles is None or len(profiles) == 0:
                    continue

                self._parse_profiles(profiles, key[1], records)
            except Exception as e:
                logger.warning(f"Failed to parse {key[0].decode()} from Redis: {e}")
                continue

        return records

    def read_key(self, key: Union[str, bytes], optional: bool) -> Optional[dict]:
        logging_key = key.decode() if isinstance(key, bytes) else key
        logger.debug(
            f"Loading profile {logging_key} at {datetime.now().timestamp()}..."
        )
        profile_data = self.client.get(key)
        if profile_data is None:
            if optional:
                logger.debug(f"No load profile for {logging_key}")
            else:
                logger.warning(f"Failed to retrieve {logging_key} from Redis")
            return None

        # Deserialize by json: dict[string]int
        try:
            profile = json.loads(profile_data.decode())
            if not isinstance(profile, dict):
                raise Exception("Load profile is not a dictionary")

            return profile
        except Exception as e:
            raise Exception(f"{e}, raw: {profile_data.decode()}")

    def progress(self) -> str:
        return ""

    def next_available(self) -> float:
        """Dataset is available to read anytime."""
        return (
            self.last_ts + self.key_ts_alignment + 2
        )  # Add 1 second to tolerate possible delay

    def _parse_profiles(
        self, profiles: dict, ts: float, out_records: List[LoadRecord] = []
    ) -> List[LoadRecord]:
        # Load metainfo.
        version = profiles.get("meta_version", 1)
        precision = profiles.get("meta_precision", 1)
        if version >= 2:
            self.key_ts_alignment = profiles.get("meta_interval_sec", 10)

        # Parse load profile entries.
        for k, v in profiles.items():
            # skip metainfos.
            if re.match(r"^meta_", k):
                continue

            # parse key: log2(input_tokens)-log2(output_tokens)
            match = re.search(r"^(\d+):(\d+)$", k)
            if match is None:
                raise Exception(f'Unexpected load profile key {k}, expect "int:int".')

            value = int(v)
            if value == 0 and v != "0":
                raise Exception(f"Load profile value is not an integer: {v}")

            input_tokens = int(match.group(1)) / precision
            output_tokens = int(match.group(2)) / precision
            out_records.append(LoadRecord(ts, input_tokens, output_tokens, value))

        return out_records
