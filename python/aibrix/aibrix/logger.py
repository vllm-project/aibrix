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

import logging
import sys
from logging import Logger
from logging.handlers import RotatingFileHandler
from pathlib import Path


def _default_logging_basic_config() -> None:
    Path("/tmp/aibrix").mkdir(parents=True, exist_ok=True)
    logging.basicConfig(
        format="%(asctime)s - %(filename)s:%(lineno)d - %(funcName)s - %(levelname)s - %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S %Z",
        handlers=[
            logging.StreamHandler(stream=sys.stdout),
            RotatingFileHandler(
                "/tmp/aibrix/python.log",
                maxBytes=10 * (2**20),
                backupCount=10,
            ),
        ],
        level=logging.INFO,
    )


def init_logger(name: str) -> Logger:
    return logging.getLogger(name)


_default_logging_basic_config()
logger = init_logger(__name__)
