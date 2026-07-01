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

import asyncio
from concurrent.futures import ThreadPoolExecutor
from concurrent.futures import TimeoutError as FutureTimeoutError

import pytest

import aibrix.client.redis as redis_client
from aibrix.storage import RedisStorage, StorageType, create_storage
from aibrix.storage.redis_upgrade import (
    REDIS_STORAGE_LATEST_VERSION,
    REDIS_STORAGE_VERSION_KEY,
    ensure_redis_storage_version,
    get_redis_storage_version,
)


def _test_redis_connectivity():
    """Test if Redis is accessible on localhost:6379."""
    try:

        def test_connection():
            async def ping():
                # Try to connect to Redis with a short timeout.
                client = redis_client.get_redis_client()
                try:
                    # Test with a simple ping against the async client.
                    return await client.ping()
                finally:
                    await client.aclose()

            return asyncio.run(ping())

        # Use ThreadPoolExecutor to enforce timeout
        with ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(test_connection)
            try:
                return future.result(timeout=5)  # 5 second timeout
            except FutureTimeoutError:
                return False
            except Exception:
                return False

    except ImportError:
        # redis package not available
        return False
    except Exception:
        return False


# Test Redis accessibility
redis_available = _test_redis_connectivity()
requires_redis = pytest.mark.skipif(
    not redis_available,
    reason="Redis not accessible - ensure Redis is running on localhost:6379 or set REDIS_HOST environment variable",
)


def get_redis_storage(**kwargs):
    """Helper to create Redis storage with environment-based configuration."""
    return create_storage(StorageType.REDIS, **kwargs)


class _FakeRedisForListObjects:
    def __init__(self, keys_in_timestamp_order: list[str]):
        self._keys = [key.encode("utf-8") for key in keys_in_timestamp_order]

    async def exists(self, _key: str) -> bool:
        return False

    async def zrange(
        self,
        key: str,
        start: int,
        end: int,
        withscores: bool = False,
    ):
        assert withscores is False
        assert key == "timestamps:all"
        if end == -1:
            return self._keys[start:]
        return self._keys[start : end + 1]


class _FakeRedisForUpgrade:
    def __init__(self):
        self.objects: dict[str, bytes] = {}
        self.values: dict[str, bytes] = {}
        self.zsets: dict[str, dict[str, float]] = {
            "timestamps:all": {
                "batch/job_001": 10.0,
                "batch/job_002": 30.0,
                "flat-key": 20.0,
            },
            "timestamps:batch": {
                "job_001": 10.0,
                "job_002": 30.0,
            },
        }

    async def get(self, key: str):
        return self.values.get(key)

    async def set(
        self,
        key: str,
        value,
        ex=None,
        px=None,
        nx: bool = False,
        xx: bool = False,
    ):
        if nx and key in self.objects:
            return None
        if xx and key not in self.objects:
            return None
        if isinstance(value, str):
            value = value.encode("utf-8")
        self.objects[key] = value
        self.values[key] = value
        return True

    async def zscore(self, key: str, member: str):
        return self.zsets.get(key, {}).get(member)

    async def zrange(self, key: str, start: int, end: int, withscores: bool = False):
        members = sorted(self.zsets.get(key, {}).items(), key=lambda item: item[1])
        if end == -1:
            sliced = members[start:]
        else:
            sliced = members[start : end + 1]
        if withscores:
            return [(member.encode("utf-8"), score) for member, score in sliced]
        return [member.encode("utf-8") for member, _ in sliced]

    async def zadd(self, key: str, mapping: dict[str, float]):
        self.zsets.setdefault(key, {}).update(mapping)
        return len(mapping)

    async def exists(self, key: str) -> bool:
        return key in self.objects or key in self.values or key in self.zsets

    async def sadd(self, key: str, member: str):
        return 1


@pytest.mark.asyncio
async def test_redis_storage_creation():
    """Test Redis storage can be created."""
    storage = RedisStorage()
    assert storage._kwargs == {}
    await storage.close()


def test_hierarchical_key_parsing():
    """Test hierarchical key parsing."""
    storage = RedisStorage()

    # Simple key
    parent, item = storage._parse_hierarchical_key("simple_key")
    assert parent is None
    assert item == "simple_key"

    # Hierarchical key
    parent, item = storage._parse_hierarchical_key("batch/job_001")
    assert parent == "batch"
    assert item == "job_001"

    # Multi-level hierarchical key
    parent, item = storage._parse_hierarchical_key("project/batch/job_001")
    assert parent == "project"
    assert item == "batch/job_001"


@pytest.mark.asyncio
async def test_multipart_not_supported():
    """Test that multipart operations raise NotImplementedError."""
    storage = RedisStorage()

    assert not storage.is_native_multipart_supported()

    with pytest.raises(NotImplementedError):
        await storage._native_create_multipart_upload("test", None, None)

    with pytest.raises(NotImplementedError):
        await storage._native_upload_part("test", "upload_id", 1, b"data")

    with pytest.raises(NotImplementedError):
        await storage._native_complete_multipart_upload("test", "upload_id", [])

    with pytest.raises(NotImplementedError):
        await storage._native_abort_multipart_upload("test", "upload_id")


@pytest.mark.asyncio
async def test_head_object_not_supported():
    """Test that head_object raises NotImplementedError."""
    storage = RedisStorage()

    with pytest.raises(NotImplementedError):
        await storage.head_object("test")


# Tests for token-based pagination functionality
def test_pagination_parameters():
    """Test that pagination parameters are accepted."""
    storage = RedisStorage()

    # Test that method signature accepts pagination parameters
    import inspect

    sig = inspect.signature(storage.list_objects)
    assert "limit" in sig.parameters
    assert "continuation_token" in sig.parameters
    assert sig.parameters["limit"].default is None
    assert sig.parameters["continuation_token"].default is None


@pytest.mark.asyncio
async def test_list_objects_returns_created_at_desc_order(monkeypatch):
    storage = RedisStorage()
    fake_redis = _FakeRedisForListObjects(
        [
            "batchjob:job-c",
            "batchjob:job-a",
            "other:key",
            "batchjob:job-b",
        ]
    )

    async def fake_get_redis():
        return fake_redis

    monkeypatch.setattr(storage, "_get_redis", fake_get_redis)

    all_keys, _ = await storage.list_objects()
    first_page, _ = await storage.list_objects(prefix="batchjob:", limit=2)
    second_page, _ = await storage.list_objects(
        prefix="batchjob:",
        limit=2,
        after_key=first_page[-1],
    )
    delimited_keys, _ = await storage.list_objects(prefix="batchjob:", delimiter=":")

    assert all_keys == [
        "batchjob:job-c",
        "batchjob:job-a",
        "other:key",
        "batchjob:job-b",
    ]
    assert first_page == ["batchjob:job-c", "batchjob:job-a"]
    assert second_page == ["batchjob:job-b"]
    assert delimited_keys == ["batchjob:job-c", "batchjob:job-a", "batchjob:job-b"]

    await storage.close()


@pytest.mark.asyncio
async def test_storage_version_defaults_to_v1_without_version_key():
    fake_redis = _FakeRedisForUpgrade()

    version = await get_redis_storage_version(fake_redis)

    assert version == 1


@pytest.mark.asyncio
async def test_ensure_redis_storage_version_upgrades_v1_indexes(monkeypatch):
    storage = RedisStorage()
    fake_redis = _FakeRedisForUpgrade()

    async def fake_get_redis():
        return fake_redis

    monkeypatch.setattr(storage, "_get_redis", fake_get_redis)

    version = await ensure_redis_storage_version(storage)

    assert version == REDIS_STORAGE_LATEST_VERSION
    assert fake_redis.values[REDIS_STORAGE_VERSION_KEY] == b"2"
    assert fake_redis.zsets["timestamps:all"] == {
        "batch/job_001": -10.0,
        "batch/job_002": -30.0,
        "flat-key": -20.0,
    }
    assert fake_redis.zsets["timestamps:batch"] == {
        "job_001": -10.0,
        "job_002": -30.0,
    }

    await storage.close()


@pytest.mark.asyncio
async def test_put_object_preserves_created_at_order_on_overwrite(monkeypatch):
    storage = RedisStorage()
    fake_redis = _FakeRedisForUpgrade()
    fake_redis.zsets = {"timestamps:all": {}}

    async def fake_get_redis():
        return fake_redis

    times = iter([100.0, 200.0, 300.0])

    monkeypatch.setattr(storage, "_get_redis", fake_get_redis)
    monkeypatch.setattr("aibrix.storage.redis.time.time", lambda: next(times))

    await storage.put_object("batchjob:job-a", b"v1")
    await storage.put_object("batchjob:job-b", b"v1")
    await storage.put_object("batchjob:job-a", b"v2")

    keys, _ = await storage.list_objects(prefix="batchjob:")

    assert fake_redis.zsets["timestamps:all"] == {
        "batchjob:job-a": -100.0,
        "batchjob:job-b": -200.0,
    }
    assert keys == ["batchjob:job-b", "batchjob:job-a"]

    await storage.close()


# Integration tests - enabled when Redis is available
@requires_redis
@pytest.mark.asyncio
async def test_redis_put_get_delete():
    """Test basic Redis operations (requires Redis running)."""
    storage = get_redis_storage()
    try:
        # Test put and get
        await storage.put_object("test_key", b"test_data")
        data = await storage.get_object("test_key")
        assert data == b"test_data"

        # Test size
        size = await storage.get_object_size("test_key")
        assert size == len(b"test_data")

        # Test exists
        exists = await storage.object_exists("test_key")
        assert exists is True

        # Test delete
        await storage.delete_object("test_key")

        # Verify deletion
        with pytest.raises(FileNotFoundError):
            await storage.get_object("test_key")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_hierarchical_operations():
    """Test hierarchical key operations (requires Redis running)."""
    storage = get_redis_storage()
    try:
        # Test hierarchical put
        await storage.put_object("batch/job_001", b"job data 1")
        await storage.put_object("batch/job_002", b"job data 2")

        # Test list operations
        objects, _ = await storage.list_objects("batch", "/")
        assert "batch/job_001" in objects
        assert "batch/job_002" in objects

        # Test get hierarchical objects
        data1 = await storage.get_object("batch/job_001")
        assert data1 == b"job data 1"

        # Test delete hierarchical objects
        await storage.delete_object("batch/job_001")
        await storage.delete_object("batch/job_002")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_default_ordering_is_created_at_desc():
    """Test that default Redis listing order is newest-first."""
    storage = get_redis_storage()
    try:
        await storage.put_object("test_key_3", b"third")
        await asyncio.sleep(0.01)  # Small delay
        await storage.put_object("test_key_1", b"first")
        await asyncio.sleep(0.01)
        await storage.put_object("test_key_2", b"second")

        objects, _ = await storage.list_objects()

        assert objects == ["test_key_2", "test_key_1", "test_key_3"]

        # Clean up
        await storage.delete_object("test_key_1")
        await storage.delete_object("test_key_2")
        await storage.delete_object("test_key_3")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_hierarchical_ordering_is_created_at_desc():
    """Test hierarchical Redis listing order is newest-first."""
    storage = get_redis_storage()
    try:
        # Put hierarchical objects with delays
        await storage.put_object("batch/job_003", b"job data 3")
        await asyncio.sleep(0.01)
        await storage.put_object("batch/job_001", b"job data 1")
        await asyncio.sleep(0.01)
        await storage.put_object("batch/job_002", b"job data 2")

        await asyncio.sleep(0.01)
        await storage.put_object("batch2/job_001", b"job data 1")

        objects, _ = await storage.list_objects("batch", "/")

        assert len(objects) == 3
        assert objects == ["batch/job_002", "batch/job_001", "batch/job_003"]

        # Clean up
        await storage.delete_object("batch/job_001")
        await storage.delete_object("batch/job_002")
        await storage.delete_object("batch/job_003")
        await storage.delete_object("batch2/job_001")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_token_pagination():
    """Test Redis token-based pagination functionality (requires Redis running)."""
    storage = get_redis_storage()
    try:
        # Clean up any existing keys first to ensure clean state
        all_existing, _ = await storage.list_objects()
        for key in all_existing:
            await storage.delete_object(key)

        # Create test objects
        for i in range(10):
            await storage.put_object(f"test_key_{i:02d}", f"data_{i}".encode())

        # Test token-based pagination
        page1, token1 = await storage.list_objects(limit=3)
        page2, token2 = await storage.list_objects(limit=3, continuation_token=token1)
        page3, _ = await storage.list_objects(limit=3, continuation_token=token2)

        # Should have 3 items each (except maybe last page)
        assert len(page1) == 3
        assert len(page2) == 3
        assert len(page3) == 3

        # Tokens should be present for first two pages
        assert token1 is not None
        assert token2 is not None
        # Last page might have more items or not

        # Should preserve the same created_at-desc order as a full listing
        all_paginated = page1 + page2 + page3
        all_objects, _ = await storage.list_objects()
        assert all_paginated[:9] == all_objects[:9]  # First 9 should match

        # Test limit without token
        limited, limited_token = await storage.list_objects(limit=5)
        assert len(limited) == 5
        assert limited == all_objects[:5]
        assert limited_token is not None  # Should have token for next page

        # Test using the limited_token
        remaining, remaining_token = await storage.list_objects(
            limit=5, continuation_token=limited_token
        )
        assert len(remaining) == 5
        assert remaining == all_objects[5:]
        assert remaining_token is None  # No more pages

        # Clean up
        for i in range(10):
            await storage.delete_object(f"test_key_{i:02d}")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_hierarchical_token_pagination():
    """Test hierarchical token-based pagination (requires Redis running)."""
    storage = get_redis_storage()
    try:
        # Create hierarchical test objects
        for i in range(10):
            await storage.put_object(f"batch/job_{i:03d}", f"job data {i}".encode())

        # Test token-based pagination on hierarchical objects
        page1, token1 = await storage.list_objects("batch", "/", limit=4)
        page2, _ = await storage.list_objects(
            "batch", "/", limit=4, continuation_token=token1
        )

        assert len(page1) == 4
        assert len(page2) == 4

        # All should be hierarchical format
        assert all(key.startswith("batch/") for key in page1)
        assert all(key.startswith("batch/") for key in page2)

        # No overlap between pages
        assert set(page1).isdisjoint(set(page2))

        # Should have token for first page, but second page might not
        assert token1 is not None

        # Clean up
        for i in range(10):
            await storage.delete_object(f"batch/job_{i:03d}")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_hierarchical_after_key_pagination():
    """Test hierarchical after_key pagination follows created_at-desc ordering."""
    storage = get_redis_storage()
    try:
        test_keys = [f"batch/after_job_{i:03d}" for i in range(4)]

        for key in test_keys:
            await storage.put_object(key, f"job data for {key}".encode())

        all_objects, _ = await storage.list_objects("batch", "/")
        first_page, _ = await storage.list_objects("batch", "/", limit=2)
        second_page, next_token = await storage.list_objects(
            "batch", "/", limit=2, after_key=first_page[-1]
        )

        assert first_page == all_objects[:2]
        assert second_page == all_objects[2:4]
        assert next_token is None

        missing_page, missing_token = await storage.list_objects(
            "batch", "/", limit=2, after_key="batch/does-not-exist"
        )
        assert missing_page == []
        assert missing_token is None

        for key in test_keys:
            await storage.delete_object(key)

    finally:
        await storage.close()


def test_feature_detection():
    """Test feature detection methods."""
    storage = RedisStorage()

    # Redis should support all advanced features
    assert storage.is_ttl_supported() is True
    assert storage.is_set_if_not_exists_supported() is True
    assert storage.is_set_if_exists_supported() is True


def test_put_object_options_validation():
    """Test PutObjectOptions validation."""
    from aibrix.storage.base import PutObjectOptions

    # Valid options
    options = PutObjectOptions()
    assert options.ttl_seconds is None
    assert options.ttl_milliseconds is None
    assert options.set_if_not_exists is False
    assert options.set_if_exists is False

    # Valid options with TTL seconds
    options = PutObjectOptions(ttl_seconds=60)
    assert options.ttl_seconds == 60

    # Valid conditional options
    options = PutObjectOptions(set_if_not_exists=True)
    assert options.set_if_not_exists is True

    # Invalid: both conditions
    with pytest.raises(
        ValueError, match="Cannot specify both set_if_not_exists and set_if_exists"
    ):
        PutObjectOptions(set_if_not_exists=True, set_if_exists=True)

    # Invalid: both TTL types
    with pytest.raises(
        ValueError, match="Cannot specify both ttl_seconds and ttl_milliseconds"
    ):
        PutObjectOptions(ttl_seconds=60, ttl_milliseconds=60000)


def test_put_object_options_builder():
    """Test PutObjectOptionsBuilder helper class."""
    from aibrix.storage.base import PutObjectOptionsBuilder

    # Test building with TTL seconds
    options = PutObjectOptionsBuilder().ttl_seconds(60).build()
    assert options.ttl_seconds == 60
    assert options.ttl_milliseconds is None

    # Test building with TTL milliseconds
    options = PutObjectOptionsBuilder().ttl_milliseconds(60000).build()
    assert options.ttl_milliseconds == 60000
    assert options.ttl_seconds is None

    # Test building with conditional operations
    options = PutObjectOptionsBuilder().if_not_exists().build()
    assert options.set_if_not_exists is True
    assert options.set_if_exists is False

    options = PutObjectOptionsBuilder().if_exists().build()
    assert options.set_if_exists is True
    assert options.set_if_not_exists is False

    # Test chaining
    options = PutObjectOptionsBuilder().ttl_seconds(300).if_not_exists().build()
    assert options.ttl_seconds == 300
    assert options.set_if_not_exists is True


@requires_redis
@pytest.mark.asyncio
async def test_redis_put_object_with_ttl():
    """Test Redis put_object with TTL options (requires Redis running)."""
    storage = get_redis_storage()
    try:
        from aibrix.storage.base import PutObjectOptions

        # Test TTL in seconds
        options = PutObjectOptions(ttl_seconds=1)  # 1 second TTL
        result = await storage.put_object("test_ttl_key", b"test_data", options=options)
        assert result is True

        # Verify data exists initially
        data = await storage.get_object("test_ttl_key")
        assert data == b"test_data"

        # Wait for TTL to expire
        await asyncio.sleep(1.1)

        # Verify data expired
        with pytest.raises(FileNotFoundError):
            await storage.get_object("test_ttl_key")

        # Test TTL in milliseconds
        options = PutObjectOptions(ttl_milliseconds=500)  # 500ms TTL
        result = await storage.put_object(
            "test_ttl_ms_key", b"test_data_ms", options=options
        )
        assert result is True

        # Verify data exists initially
        data = await storage.get_object("test_ttl_ms_key")
        assert data == b"test_data_ms"

        # Wait for TTL to expire
        await asyncio.sleep(0.6)

        # Verify data expired
        with pytest.raises(FileNotFoundError):
            await storage.get_object("test_ttl_ms_key")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_put_object_conditional():
    """Test Redis put_object conditional operations (requires Redis running)."""
    storage = get_redis_storage()
    try:
        from aibrix.storage.base import PutObjectOptions

        key = "test_conditional_key"

        # Ensure key doesn't exist
        await storage.delete_object(key)

        # Test SET IF NOT EXISTS (NX) - should succeed
        options = PutObjectOptions(set_if_not_exists=True)
        result = await storage.put_object(key, b"first_value", options=options)
        assert result is True

        # Verify data was set
        data = await storage.get_object(key)
        assert data == b"first_value"

        # Test SET IF NOT EXISTS again - should fail since key exists
        result = await storage.put_object(key, b"second_value", options=options)
        assert result is False

        # Verify data unchanged
        data = await storage.get_object(key)
        assert data == b"first_value"

        # Test SET IF EXISTS (XX) - should succeed since key exists
        options = PutObjectOptions(set_if_exists=True)
        result = await storage.put_object(key, b"updated_value", options=options)
        assert result is True

        # Verify data was updated
        data = await storage.get_object(key)
        assert data == b"updated_value"

        # Delete key and test SET IF EXISTS - should fail
        await storage.delete_object(key)
        result = await storage.put_object(key, b"should_fail", options=options)
        assert result is False

        # Verify key doesn't exist
        with pytest.raises(FileNotFoundError):
            await storage.get_object(key)

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_put_object_combined_options():
    """Test Redis put_object with combined TTL and conditional options (requires Redis running)."""
    storage = get_redis_storage()
    try:
        from aibrix.storage.base import PutObjectOptionsBuilder

        key = "test_combined_key"

        # Ensure key doesn't exist
        await storage.delete_object(key)

        # Test NX with TTL
        options = PutObjectOptionsBuilder().ttl_seconds(2).if_not_exists().build()

        result = await storage.put_object(key, b"ttl_nx_value", options=options)
        assert result is True

        # Verify data exists
        data = await storage.get_object(key)
        assert data == b"ttl_nx_value"

        # Try to set again with NX - should fail
        result = await storage.put_object(key, b"should_fail", options=options)
        assert result is False

        # Wait for TTL to expire
        await asyncio.sleep(2.1)

        # Verify data expired
        with pytest.raises(FileNotFoundError):
            await storage.get_object(key)

    finally:
        await storage.close()
