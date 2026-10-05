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

from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Any, Dict

from .. import envs
from ..status import Status


@dataclass
class MetaServiceConfig(ABC):
    url: str
    cluster_meta_key: str


class MetaService(ABC):
    """MetaService API."""

    def __init__(self, config: MetaServiceConfig) -> None:
        self.config = config

    @staticmethod
    def create(
        backend_name: str,
    ) -> "MetaService":
        """Create a connection to meta service."""
        config = MetaServiceConfig(
            url=envs.AIBRIX_KV_CACHE_OL_META_SERVICE_URL,
            cluster_meta_key=envs.AIBRIX_KV_CACHE_OL_META_SERVICE_CLUSTER_META_KEY,
        )
        if backend_name == "REDIS":
            from .redis_meta_service import RedisMetaService

            return RedisMetaService.from_envs(config)
        else:
            raise ValueError(f"Unknown type: {backend_name}")

    @classmethod
    @abstractmethod
    def from_envs(cls, config: MetaServiceConfig):
        """Create a meta service connection from env variables."""
        raise NotImplementedError

    @property
    @abstractmethod
    def name(self) -> str:
        raise NotImplementedError

    @abstractmethod
    def open(self) -> Status:
        """Open the connection to the meta service."""
        raise NotImplementedError

    @abstractmethod
    def close(self) -> Status:
        """Close the connection to the meta service."""
        raise NotImplementedError

    @abstractmethod
    def get_cluster_metadata(self) -> Status[str]:
        """Retrieve cluster metadata."""
        raise NotImplementedError

    def get_connection_info(self) -> Dict[str, Any]:
        raise NotImplementedError
