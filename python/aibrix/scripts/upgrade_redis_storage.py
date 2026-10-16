#!/usr/bin/env python3

import asyncio

from aibrix.logger import init_logger
from aibrix.storage import RedisStorage, StorageType, create_storage
from aibrix.storage.redis_upgrade import (
    REDIS_STORAGE_LATEST_VERSION,
    ensure_redis_storage_version,
)

logger = init_logger(__name__)


async def _main() -> int:
    storage = create_storage(StorageType.REDIS)
    if not isinstance(storage, RedisStorage):
        raise RuntimeError(
            "create_storage(StorageType.REDIS) did not return RedisStorage"
        )

    try:
        version = await ensure_redis_storage_version(storage)
        logger.info(
            "Redis storage upgrade completed",
            version=version,
            latest=REDIS_STORAGE_LATEST_VERSION,
        )  # type: ignore[call-arg]
        return 0
    finally:
        await storage.close()


def main() -> int:
    return asyncio.run(_main())


if __name__ == "__main__":
    raise SystemExit(main())
