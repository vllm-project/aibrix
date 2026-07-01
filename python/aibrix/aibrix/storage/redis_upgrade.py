import math
from typing import Any, Iterable, Optional

from aibrix.logger import init_logger
from aibrix.storage.redis import RedisStorage

logger = init_logger(__name__)

REDIS_STORAGE_VERSION_KEY = "storage:version"
REDIS_STORAGE_VERSION_V1 = 1
REDIS_STORAGE_VERSION_V2 = 2
REDIS_STORAGE_LATEST_VERSION = REDIS_STORAGE_VERSION_V2
_ZADD_CHUNK_SIZE = 1000


def _decode_redis_value(value: Any) -> Optional[str]:
    if value is None:
        return None
    if isinstance(value, bytes):
        return value.decode("utf-8")
    return str(value)


def _normalize_desc_score(score: float) -> float:
    if math.isnan(score):
        raise ValueError("Redis storage index contains NaN score")
    return -abs(float(score))


def _chunk_mapping(
    mapping: dict[str, float], chunk_size: int
) -> Iterable[dict[str, float]]:
    items = list(mapping.items())
    for start in range(0, len(items), chunk_size):
        yield dict(items[start : start + chunk_size])


async def get_redis_storage_version(redis_client: Any) -> int:
    raw_version = _decode_redis_value(await redis_client.get(REDIS_STORAGE_VERSION_KEY))
    if raw_version is None:
        return REDIS_STORAGE_VERSION_V1
    try:
        return int(raw_version)
    except ValueError as exc:
        raise ValueError(
            f"Invalid Redis storage version value: {raw_version!r}"
        ) from exc


async def ensure_redis_storage_version(storage: RedisStorage) -> int:
    redis_client = await storage._get_redis()
    current_version = await get_redis_storage_version(redis_client)
    if current_version >= REDIS_STORAGE_LATEST_VERSION:
        return current_version

    logger.info(
        "Upgrading Redis storage indexes",
        from_version=current_version,
        to_version=REDIS_STORAGE_LATEST_VERSION,
    )  # type: ignore[call-arg]

    if current_version == REDIS_STORAGE_VERSION_V1:
        await upgrade_redis_storage_v1_to_v2(redis_client)
    else:
        raise RuntimeError(f"Unsupported Redis storage version: {current_version}")

    await redis_client.set(
        REDIS_STORAGE_VERSION_KEY, str(REDIS_STORAGE_LATEST_VERSION).encode("utf-8")
    )
    logger.info(
        "Redis storage indexes upgraded",
        to_version=REDIS_STORAGE_LATEST_VERSION,
    )  # type: ignore[call-arg]
    return REDIS_STORAGE_LATEST_VERSION


async def maybe_upgrade_storage(storage: Any) -> Optional[int]:
    if not isinstance(storage, RedisStorage):
        return None
    return await ensure_redis_storage_version(storage)


async def upgrade_redis_storage_v1_to_v2(redis_client: Any) -> None:
    # Rewrite the global key index from positive timestamps (oldest first) to
    # negative timestamps so the existing ascending pagination yields newest first.
    timestamp_entries = await redis_client.zrange(
        "timestamps:all", 0, -1, withscores=True
    )

    global_mapping: dict[str, float] = {}
    parent_mappings: dict[str, dict[str, float]] = {}

    for member, score in timestamp_entries:
        key = _decode_redis_value(member)
        if key is None:
            continue
        normalized_score = _normalize_desc_score(float(score))
        global_mapping[key] = normalized_score

        if "/" not in key:
            continue
        parent_key, item_key = key.split("/", 1)
        parent_mappings.setdefault(parent_key, {})[item_key] = normalized_score

    for mapping in _chunk_mapping(global_mapping, _ZADD_CHUNK_SIZE):
        await redis_client.zadd("timestamps:all", mapping)

    for parent_key, mapping in parent_mappings.items():
        timestamp_key = f"timestamps:{parent_key}"
        for chunk in _chunk_mapping(mapping, _ZADD_CHUNK_SIZE):
            await redis_client.zadd(timestamp_key, chunk)
