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

import os
from typing import List, Optional

ENV_VARS_TRUE_VALUES = {"1", "ON", "YES", "TRUE"}


def _is_true(value: Optional[str]) -> bool:
    if value is None:
        return False
    return value.upper() in ENV_VARS_TRUE_VALUES


def _parse_list_str(value: Optional[str], sep: str = ",") -> Optional[List[str]]:
    if value is None:
        return None
    return [str(item).strip() for item in value.split(sep)]


def _parse_int_or_none(value: Optional[str]) -> Optional[int]:
    if value is None:
        return None
    return int(value)


# Model Download Related Config

# Downloader Default Directory
DOWNLOADER_LOCAL_DIR = os.getenv("DOWNLOADER_LOCAL_DIR", "/tmp/aibrix/models/")


DOWNLOADER_NUM_THREADS = int(os.getenv("DOWNLOADER_NUM_THREADS", "4"))
DOWNLOADER_PART_THRESHOLD = _parse_int_or_none(os.getenv("DOWNLOADER_PART_THRESHOLD"))
DOWNLOADER_PART_CHUNKSIZE = _parse_int_or_none(os.getenv("DOWNLOADER_PART_CHUNKSIZE"))
DOWNLOADER_ALLOW_FILE_SUFFIX = _parse_list_str(
    os.getenv("DOWNLOADER_ALLOW_FILE_SUFFIX")
)

DOWNLOADER_FORCE_DOWNLOAD = _is_true(os.getenv("DOWNLOADER_FORCE_DOWNLOAD", "0"))
DOWNLOADER_CHECK_FILE_EXIST = _is_true(os.getenv("DOWNLOADER_CHECK_FILE_EXIST", "1"))

# Downloader Regex
DOWNLOADER_S3_REGEX = r"^s3://"
DOWNLOADER_TOS_REGEX = r"^tos://"

# Downloader HuggingFace Envs
DOWNLOADER_HF_TOKEN = os.getenv("HF_TOKEN")
DOWNLOADER_HF_ENDPOINT = os.getenv("HF_ENDPOINT")
DOWNLOADER_HF_REVISION = os.getenv("HF_REVISION")

# Downloader TOS Envs
DOWNLOADER_TOS_ACCESS_KEY = os.getenv("TOS_ACCESS_KEY")
DOWNLOADER_TOS_SECRET_KEY = os.getenv("TOS_SECRET_KEY")
DOWNLOADER_TOS_ENDPOINT = os.getenv("TOS_ENDPOINT")
DOWNLOADER_TOS_REGION = os.getenv("TOS_REGION")
DOWNLOADER_TOS_ENABLE_CRC = _is_true(os.getenv("TOS_ENABLE_CRC"))

# Downloader AWS S3 Envs
DOWNLOADER_AWS_ACCESS_KEY_ID = os.getenv("AWS_ACCESS_KEY_ID")
DOWNLOADER_AWS_SECRET_ACCESS_KEY = os.getenv("AWS_SECRET_ACCESS_KEY")
DOWNLOADER_AWS_ENDPOINT_URL = os.getenv("AWS_ENDPOINT_URL")
DOWNLOADER_AWS_REGION = os.getenv("AWS_REGION")

# Metric Standardizing Related Config
# Scrape config
METRIC_SCRAPE_PATH = os.getenv("METRIC_SCRAPE_PATH", "/metrics")

# Runtime Metric config
PROMETHEUS_MULTIPROC_DIR = os.getenv("PROMETHEUS_MULTIPROC_DIR", "/tmp/aibrix/metrics/")

# Inference Engine Config
INFERENCE_ENGINE = os.getenv("INFERENCE_ENGINE", "vllm")
INFERENCE_ENGINE_VERSION = os.getenv("INFERENCE_ENGINE_VERSION", "0.6.1")
INFERENCE_ENGINE_ENDPOINT = os.getenv(
    "INFERENCE_ENGINE_ENDPOINT", "http://localhost:8000"
)
