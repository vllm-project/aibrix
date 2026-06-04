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

from datetime import datetime, timedelta
from typing import AbstractSet, Any, Callable, Mapping, Optional, Protocol, cast

from aibrix import envs
from aibrix.logger import init_logger

from .utils import _require_setting

logger = init_logger(__name__)


class RedisPipeline(Protocol):
    def get(self, name: bytes | str | memoryview) -> Any: ...

    def smembers(self, name: bytes | str | memoryview) -> Any: ...

    def set(
        self,
        name: bytes | str | memoryview,
        value: bytes | bytearray | memoryview | str | int | float,
        ex: int | timedelta | None = None,
        px: int | timedelta | None = None,
        nx: bool = False,
        xx: bool = False,
        keepttl: bool = False,
        get: bool = False,
        exat: int | datetime | None = None,
        pxat: int | datetime | None = None,
        ifeq: str | bytes | None = None,
        ifne: str | bytes | None = None,
        ifdeq: Optional[str] = None,
        ifdne: Optional[str] = None,
    ) -> Any: ...

    def zadd(
        self,
        name: bytes | str | memoryview,
        mapping: Mapping[Any, bytes | bytearray | memoryview | str | int | float],
        nx: bool = False,
        xx: bool = False,
        ch: bool = False,
        incr: bool = False,
        gt: bool = False,
        lt: bool = False,
    ) -> Any: ...

    def sadd(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    def delete(self, *names: bytes | str | memoryview) -> Any: ...

    def zrem(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    def srem(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    async def execute(self, raise_on_error: bool = True) -> list[Any]: ...


class AsyncRedis(Protocol):
    async def get(self, name: bytes | str | memoryview) -> Optional[bytes | str]: ...

    async def set(
        self,
        name: bytes | str | memoryview,
        value: bytes | bytearray | memoryview | str | int | float,
        ex: int | timedelta | None = None,
        px: int | timedelta | None = None,
        nx: bool = False,
        xx: bool = False,
        keepttl: bool = False,
        get: bool = False,
        exat: int | datetime | None = None,
        pxat: int | datetime | None = None,
        ifeq: str | bytes | None = None,
        ifne: str | bytes | None = None,
        ifdeq: Optional[str] = None,
        ifdne: Optional[str] = None,
    ) -> Any: ...

    async def exists(self, *names: bytes | str | memoryview) -> Any: ...

    async def delete(self, *names: bytes | str | memoryview) -> Any: ...

    async def ping(self) -> Any: ...

    async def zadd(
        self,
        name: bytes | str | memoryview,
        mapping: Mapping[Any, bytes | bytearray | memoryview | str | int | float],
        nx: bool = False,
        xx: bool = False,
        ch: bool = False,
        incr: bool = False,
        gt: bool = False,
        lt: bool = False,
    ) -> Any: ...

    async def zrange(
        self,
        name: bytes | str | memoryview,
        start: bytes | bytearray | memoryview | str | int | float,
        end: bytes | bytearray | memoryview | str | int | float,
        desc: bool = False,
        withscores: bool = False,
        score_cast_func: type | Any = float,
        byscore: bool = False,
        bylex: bool = False,
        offset: Optional[int] = None,
        num: Optional[int] = None,
    ) -> list[Any]: ...

    async def zrevrange(
        self,
        name: bytes | str | memoryview,
        start: int,
        end: int,
        withscores: bool = False,
        score_cast_func: type | Any = float,
    ) -> list[Any]: ...

    async def zrevrank(
        self,
        name: bytes | str | memoryview,
        value: bytes | bytearray | memoryview | str | int | float,
    ) -> Optional[int]: ...

    async def zrem(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    async def sadd(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    async def srem(
        self,
        name: bytes | str | memoryview,
        *values: bytes | bytearray | memoryview | str | int | float,
    ) -> Any: ...

    async def smembers(self, name: bytes | str | memoryview) -> AbstractSet[Any]: ...

    async def strlen(self, key: bytes | str | memoryview) -> int: ...

    async def aclose(self) -> None: ...

    def pipeline(
        self, transaction: bool = True, shard_hint: Optional[str] = None
    ) -> RedisPipeline: ...


async def run_pipeline(
    client: AsyncRedis, callback: Callable[[RedisPipeline], Any]
) -> list[Any]:
    pipeline = client.pipeline()
    callback(pipeline)
    return await pipeline.execute()


def get_redis_client(require_check: bool = False, **kwargs) -> AsyncRedis:
    import redis.asyncio as redis

    resolved_host = _require_setting(
        "REDIS_HOST",
        kwargs.get("host")
        or envs.STORAGE_REDIS_HOST
        or (None if require_check else "localhost"),
    )
    resolved_port = kwargs.get("port") or envs.STORAGE_REDIS_PORT
    resolved_db = (
        cast(int, kwargs.get("db"))
        if kwargs.get("db") is not None
        else envs.STORAGE_REDIS_DB
    )
    resolved_password = kwargs.get("password") or envs.STORAGE_REDIS_PASSWORD
    logger.info(  # type: ignore[call-arg]
        "Creating Redis client",
        host=resolved_host,
        port=resolved_port,
        db=resolved_db,
        password_configured=resolved_password is not None,
        require_check=require_check,
        extra_kwargs=sorted(
            key for key in kwargs if key not in {"host", "port", "db", "password"}
        ),
    )
    return cast(
        AsyncRedis,
        redis.Redis(
            host=resolved_host,
            port=resolved_port,
            db=resolved_db,
            password=resolved_password,
            **{
                key: value
                for key, value in kwargs.items()
                if key not in {"host", "port", "db", "password"}
            },
        ),
    )
