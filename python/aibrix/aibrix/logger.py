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
import os
import sys
from logging import Logger
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import Optional

import structlog

from .config import AIBrixSettings

active_settings: AIBrixSettings = AIBrixSettings()


def logging_basic_config(settings: Optional[AIBrixSettings] = None) -> None:
    global active_settings
    if settings is not None:
        active_settings = settings

    # 1. Configure the standard library logging
    handlers: list[logging.Handler] = [logging.StreamHandler(stream=sys.stdout)]
    if active_settings.LOG_PATH is not None:
        Path(os.path.dirname(active_settings.LOG_PATH)).mkdir(
            parents=True, exist_ok=True
        )
        handlers.append(
            RotatingFileHandler(
                active_settings.LOG_PATH,
                maxBytes=10 * (2**20),
                backupCount=10,
            )
        )
    logging.basicConfig(format=active_settings.LOG_FORMAT, handlers=handlers)

    # 2. Configure structlog processors and renderer
    structlog.configure(
        processors=[
            structlog.stdlib.add_logger_name,
            structlog.stdlib.add_log_level,
            structlog.processors.TimeStamper(fmt="%Y-%m-%d %H:%M:%S %Z"),
            structlog.processors.StackInfoRenderer(),
            structlog.processors.format_exc_info,
            structlog.processors.JSONRenderer(),  # Renders the log event as JSON
        ],
        logger_factory=structlog.stdlib.LoggerFactory(),
        wrapper_class=structlog.stdlib.BoundLogger,
        cache_logger_on_first_use=True,
    )


def init_logger(name: str) -> Logger:
    logger = structlog.get_logger(name)
    logger.setLevel(active_settings.LOG_LEVEL)
    return logger


logging_basic_config()
logger = init_logger(__name__)
