import math
from typing import Any, Iterable, Optional

from aibrix.logger import init_logger
from aibrix.storage.redis import RedisStorage

logger = init_logger(__name__)

REDIS_STORAGE_VERSION_KEY = "storage:version"
REDIS_STORAGE_VERSION_V1 = 1
REDIS_STORAGE_VERSION_V2 = 2
REDIS_STORAGE_VERSION_V3 = 3
REDIS_STORAGE_LATEST_VERSION = REDIS_STORAGE_VERSION_V3
_ZADD_CHUNK_SIZE = 1000
_SADD_CHUNK_SIZE = 1000
_VERIFY_ZRANGE_CHUNK_SIZE = 1000
_OLD_BATCH_JOB_PREFIX = "batchjob:"
_NEW_BATCH_JOB_PREFIX = "batchjob/"
_OLD_BATCH_STATUS_COPIES_PREFIX = "batchstatus_copies:"


def _decode_redis_value(value: Any) -> Optional[str]:
    if value is None:
        return None
    if isinstance(value, bytes):
        return value.decode("utf-8")
    return str(value)


def _normalize_created_at_score(score: float) -> float:
    if math.isnan(score):
        raise ValueError("Redis storage index contains NaN score")
    return abs(float(score))


def _chunk_mapping(
    mapping: dict[str, float], chunk_size: int
) -> Iterable[dict[str, float]]:
    items = list(mapping.items())
    for start in range(0, len(items), chunk_size):
        yield dict(items[start : start + chunk_size])


def _chunk_items(items: Iterable[str], chunk_size: int) -> Iterable[list[str]]:
    buffered: list[str] = []
    for item in items:
        buffered.append(item)
        if len(buffered) == chunk_size:
            yield buffered
            buffered = []
    if buffered:
        yield buffered


async def _iter_zrange_withscores(
    redis_client: Any,
    key: str,
    chunk_size: int = _VERIFY_ZRANGE_CHUNK_SIZE,
):
    offset = 0
    while True:
        entries = await redis_client.zrange(
            key,
            offset,
            offset + chunk_size - 1,
            withscores=True,
        )
        if not entries:
            break
        for entry in entries:
            yield entry
        if len(entries) < chunk_size:
            break
        offset += len(entries)


def _remap_batch_metastore_key_to_v3(key: str) -> str:
    if key.startswith(_OLD_BATCH_JOB_PREFIX):
        return f"{_NEW_BATCH_JOB_PREFIX}{key.removeprefix(_OLD_BATCH_JOB_PREFIX)}"

    if key.startswith(_OLD_BATCH_STATUS_COPIES_PREFIX):
        remainder = key.removeprefix(_OLD_BATCH_STATUS_COPIES_PREFIX)
        batch_id, separator, worker_id = remainder.rpartition(":")
        if separator and batch_id and worker_id:
            return f"{_OLD_BATCH_STATUS_COPIES_PREFIX}{batch_id}/{worker_id}"

    return key


def _status_copy_parent_keys_for_cleanup(*keys: str) -> set[str]:
    parent_keys: set[str] = set()
    for key in keys:
        if not key.startswith(_OLD_BATCH_STATUS_COPIES_PREFIX):
            continue
        slash_index = key.find("/")
        while slash_index != -1:
            parent_keys.add(key[:slash_index])
            slash_index = key.find("/", slash_index + 1)
    return parent_keys


async def verify_redis_storage_v3(redis_client: Any) -> None:
    """Validate that Redis storage indexes match the v3 invariants.

    Raises:
        RuntimeError: If any migrated key, score, or parent index is inconsistent
    """
    expected_parent_scores: dict[str, dict[str, float]] = {}
    expected_parent_members: dict[str, set[str]] = {}

    async for member, score in _iter_zrange_withscores(redis_client, "timestamps:all"):
        key = _decode_redis_value(member)
        if key is None:
            continue

        normalized_score = _normalize_created_at_score(float(score))
        if float(score) != normalized_score:
            raise RuntimeError(
                f"Redis storage v3 verification failed: negative timestamp score for {key!r}"
            )

        migrated_key = _remap_batch_metastore_key_to_v3(key)
        if migrated_key != key:
            raise RuntimeError(
                f"Redis storage v3 verification failed: legacy key still present {key!r}"
            )

        if key.startswith(_OLD_BATCH_STATUS_COPIES_PREFIX):
            raise RuntimeError(
                "Redis storage v3 verification failed: batch status copy entries "
                f"must be removed during upgrade, found {key!r}"
            )

        if "/" not in key:
            continue

        parent_key, item_key = key.split("/", 1)
        expected_parent_scores.setdefault(parent_key, {})[item_key] = normalized_score
        expected_parent_members.setdefault(parent_key, set()).add(item_key)

    for parent_key, expected_members in expected_parent_members.items():
        actual_members_raw = await redis_client.smembers(f"{parent_key}:index")
        actual_members = {
            value
            for member in actual_members_raw
            if (value := _decode_redis_value(member)) is not None
        }
        if actual_members != expected_members:
            raise RuntimeError(
                "Redis storage v3 verification failed: parent index mismatch for "
                f"{parent_key!r}, expected {sorted(expected_members)!r}, got {sorted(actual_members)!r}"
            )

        actual_scores: dict[str, float] = {}
        async for member, score in _iter_zrange_withscores(
            redis_client, f"timestamps:{parent_key}"
        ):
            decoded_item_key = _decode_redis_value(member)
            if decoded_item_key is None:
                continue
            normalized_score = _normalize_created_at_score(float(score))
            if float(score) != normalized_score:
                raise RuntimeError(
                    "Redis storage v3 verification failed: negative parent timestamp "
                    f"score for {parent_key!r}/{decoded_item_key!r}"
                )
            actual_scores[decoded_item_key] = normalized_score

        expected_scores = expected_parent_scores[parent_key]
        if actual_scores != expected_scores:
            raise RuntimeError(
                "Redis storage v3 verification failed: parent timestamp mismatch for "
                f"{parent_key!r}, expected {expected_scores!r}, got {actual_scores!r}"
            )


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

    if current_version in (REDIS_STORAGE_VERSION_V1, REDIS_STORAGE_VERSION_V2):
        await upgrade_redis_storage_to_v3(redis_client)
        current_version = REDIS_STORAGE_VERSION_V3
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


async def upgrade_redis_storage_to_v3(redis_client: Any) -> None:
    # Normalize all Redis timestamp indexes to positive created_at scores,
    # rekey batch metastore entries to slash-delimited hierarchy, and rebuild
    # hierarchical parent indexes from the canonical global timestamp set.
    timestamp_entries = await redis_client.zrange(
        "timestamps:all", 0, -1, withscores=True
    )

    global_mapping: dict[str, float] = {}
    parent_mappings: dict[str, dict[str, float]] = {}
    parent_members: dict[str, set[str]] = {}
    status_copy_parent_keys: set[str] = set()

    for member, score in timestamp_entries:
        key = _decode_redis_value(member)
        if key is None:
            continue
        migrated_key = _remap_batch_metastore_key_to_v3(key)
        normalized_score = _normalize_created_at_score(float(score))
        if migrated_key.startswith(_OLD_BATCH_STATUS_COPIES_PREFIX):
            status_copy_parent_keys.update(
                _status_copy_parent_keys_for_cleanup(key, migrated_key)
            )
            if key != migrated_key:
                await redis_client.delete(key, migrated_key)
            else:
                await redis_client.delete(key)
            continue

        if migrated_key != key:
            payload = await redis_client.get(key)
            if payload is not None:
                await redis_client.set(migrated_key, payload)
                await redis_client.delete(key)
            elif not await redis_client.exists(migrated_key):
                logger.warning(
                    "Skipping missing batch metastore key during Redis v3 upgrade",
                    key=key,
                    migrated_key=migrated_key,
                )  # type: ignore[call-arg]

        global_mapping[migrated_key] = normalized_score

        if "/" not in migrated_key:
            continue

        parent_key, item_key = migrated_key.split("/", 1)
        parent_mappings.setdefault(parent_key, {})[item_key] = normalized_score
        parent_members.setdefault(parent_key, set()).add(item_key)

    await redis_client.delete("timestamps:all")
    for score_chunk in _chunk_mapping(global_mapping, _ZADD_CHUNK_SIZE):
        await redis_client.zadd("timestamps:all", score_chunk)

    for parent_key in status_copy_parent_keys:
        await redis_client.delete(f"{parent_key}:index", f"timestamps:{parent_key}")

    for parent_key, mapping in parent_mappings.items():
        await redis_client.delete(f"{parent_key}:index", f"timestamps:{parent_key}")
        for member_chunk in _chunk_items(
            sorted(parent_members[parent_key]), _SADD_CHUNK_SIZE
        ):
            await redis_client.sadd(f"{parent_key}:index", *member_chunk)
        for score_chunk in _chunk_mapping(mapping, _ZADD_CHUNK_SIZE):
            await redis_client.zadd(f"timestamps:{parent_key}", score_chunk)

    await verify_redis_storage_v3(redis_client)
