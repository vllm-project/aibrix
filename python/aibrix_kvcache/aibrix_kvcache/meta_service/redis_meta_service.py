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
from typing import Any, Dict

import redis

from ..common.absl_logging import getLogger, log_every_n_seconds
from ..status import Status, StatusCodes
from . import MetaService, MetaServiceConfig

logger = getLogger(__name__)


class RedisMetaService(MetaService):
    """MetaService implemented by redis."""

    def __init__(self, config: MetaServiceConfig):
        super().__init__(config)
        self.client: redis.Redis | None = None

    @classmethod
    def from_envs(cls, config: MetaServiceConfig) -> "RedisMetaService":
        """Create a meta service connection from env variables."""
        return cls(config)

    @property
    def name(self) -> str:
        return "Redis"

    def open(self) -> Status:
        try:
            self.client = redis.Redis.from_url(url=self.config.url)
            assert self.client is not None
            self.client.ping()
        except redis.RedisError as e:
            logger.error("Redis connection failed: %s", e)
            return Status(StatusCodes.ERROR, f"Redis connection failed: {e}")

        return Status.ok()

    def close(self) -> Status:
        try:
            if self.client is not None:
                self.client.close()
                self.client = None
            return Status.ok()
        except redis.ConnectionError as e:
            logger.error("Redis close failed: %s", e)
            return Status(StatusCodes.ERROR, f"Redis close failed: {e}")

    def get_cluster_metadata(self) -> Status[str]:
        assert self.client is not None
        try:
            key = self.config.cluster_meta_key
            json_str = self.client.get(key)  # type: ignore
            if json_str is None:
                Status(StatusCodes.ERROR, f"No data at Redis key: {key}")
            return Status.ok(json_str.decode("utf-8"))  # type: ignore
        except (redis.RedisError, UnicodeDecodeError) as e:
            log_every_n_seconds(
                logger, logging.ERROR, "Redis get failed: %s", 10, e
            )
            return Status(StatusCodes.ERROR, f"Redis get failed: {e}")

    def get_connection_info(self) -> Dict[str, Any]:
        assert self.client is not None
        try:
            return {
                "backend": "redis",
                "connected": self.client.ping(),
                "version": self.client.info().get("redis_version"),  # type: ignore
                "key": self.config.cluster_meta_key,
            }
        except redis.RedisError:
            return {"backend": "redis", "connected": False}
