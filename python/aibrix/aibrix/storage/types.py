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

from enum import Enum
from typing import Optional, Any, Protocol, Callable, AbstractSet

class StorageType(Enum):
    """Supported storage types."""

    LOCAL = "local"
    S3 = "s3"
    TOS = "tos"
    REDIS = "redis"
    AUTO = "auto"

class RedisPipeline(Protocol):
    def zadd(self, key: str, mapping: dict[str, float]) -> Any: ...

    def sadd(self, key: str, value: str) -> Any: ...

    def delete(self, key: str) -> Any: ...

    def zrem(self, key: str, value: str) -> Any: ...

    def srem(self, key: str, value: str) -> Any: ...

class AsyncRedis(Protocol):
    async def get(self, key: str) -> Optional[bytes]: ...

    async def set(
        self,
        key: str,
        value: bytes | str,
        ex: Optional[int] = None,
        px: Optional[int] = None,
        nx: bool = False,
        xx: bool = False,
    ) -> Any: ...

    async def exists(self, key: str) -> Any: ...

    async def delete(self, key: str) -> Any: ...

    async def ping(self) -> Any: ...

    async def zadd(self, key: str, mapping: dict[str, float]) -> Any: ...

    async def zrange(
        self, key: str, start: int, end: int, withscores: bool = False
    ) -> list[bytes]: ...

    async def zrevrange(self, key: str, start: int, end: int) -> list[bytes | str]: ...

    async def zrevrank(self, key: str, value: str) -> Optional[int]: ...

    async def zrem(self, key: str, value: str) -> Any: ...

    async def smembers(self, key: str) -> AbstractSet[bytes]: ...

    async def strlen(self, key: str) -> int: ...

    async def aclose(self) -> None: ...

    async def run_pipeline(
        self, callback: Callable[[RedisPipeline], None]
    ) -> list[Any]: ...