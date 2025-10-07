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
from typing import Optional

from pydantic_settings import BaseSettings, SettingsConfigDict


class AIBrixSettings(BaseSettings):
    # This loads *only* from system environment variables
    # Uncomment env_file and env_file_encoding to load from .env file
    model_config = SettingsConfigDict(
        # env_file=".env",  # Load environment variables from a .env file
        # env_file_encoding="utf-8",  # Encoding for the .env file
        extra="ignore",
    )

    # --- Security Settings ---
    SECRET_KEY: str = (
        "test-secret-key-for-testing"  # No default, reserved for later use.
    )

    # --- Logging Settings ---
    LOG_LEVEL: str = "DEBUG"  # DEBUG, INFO, WARNING, ERROR, CRITICAL
    LOG_PATH: Optional[str] = "/tmp/aibrix/python.log"  # If None, logs to stdout only
    LOG_FORMAT: str = "%(asctime)s - %(filename)s:%(lineno)d - %(funcName)s - %(levelname)s - %(message)s"


DEFAULT_METRIC_COLLECTOR_TIMEOUT = 1
"""
DOWNLOAD CACHE DIR would be like:
.
└── .cache
    └── huggingface | s3 | tos
        ├── .gitignore
        └── download
"""
DOWNLOAD_CACHE_DIR = ".cache/%s/download"
DOWNLOAD_FILE_LOCK_CHECK_TIMEOUT = 10

EXCLUDE_METRICS_HTTP_ENDPOINTS = ["/metrics/"]
