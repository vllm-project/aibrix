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
import os

import pytest

from aibrix.storage import RedisStorage, StorageType, create_storage

# Skip integration tests if Redis is not configured
redis_available = os.environ.get("REDIS_HOST") is not None
requires_redis = pytest.mark.skipif(
    not redis_available,
    reason="Redis not available - set STORAGE_REDIS_HOST environment variable to enable Redis tests",
)

def get_redis_storage(**kwargs):
    """Helper to create Redis storage with environment-based configuration."""
    return create_storage(StorageType.REDIS, **kwargs)


@pytest.mark.asyncio
async def test_redis_storage_creation():
    """Test Redis storage can be created."""
    storage = RedisStorage()
    assert storage.host == "localhost"
    assert storage.port == 6379
    assert storage.db == 0
    assert storage.password is None
    await storage.close()


@pytest.mark.asyncio
async def test_redis_storage_creation_with_params():
    """Test Redis storage can be created with custom parameters."""
    storage = RedisStorage(host="redis-server", port=6380, db=1, password="secret")
    assert storage.host == "redis-server"
    assert storage.port == 6380
    assert storage.db == 1
    assert storage.password == "secret"
    await storage.close()


@pytest.mark.asyncio
async def test_redis_storage_factory():
    """Test Redis storage can be created via factory."""
    storage = create_storage(StorageType.REDIS, host="localhost", port=6379, db=0)
    assert isinstance(storage, RedisStorage)
    assert storage.host == "localhost"
    assert storage.port == 6379
    assert storage.db == 0
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

    with pytest.raises(NotImplementedError):
        await storage.create_multipart_upload("test", None, None)

    with pytest.raises(NotImplementedError):
        await storage.upload_part("test", "upload_id", 1, b"data")

    with pytest.raises(NotImplementedError):
        await storage.complete_multipart_upload("test", "upload_id", [])

    with pytest.raises(NotImplementedError):
        await storage.abort_multipart_upload("test", "upload_id")


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
async def test_redis_timestamp_ordering():
    """Test that list_objects returns keys ordered by creation timestamp (requires Redis running)."""
    storage = get_redis_storage()
    try:
        # Put objects with slight delays to ensure different timestamps
        await storage.put_object("test_key_3", b"third")
        await asyncio.sleep(0.01)  # Small delay
        await storage.put_object("test_key_1", b"first")
        await asyncio.sleep(0.01)
        await storage.put_object("test_key_2", b"second")

        # List all objects - should be ordered by creation time
        objects, _ = await storage.list_objects()

        # Should be ordered by creation timestamp: test_key_3, test_key_1, test_key_2
        assert objects.index("test_key_3") < objects.index("test_key_1")
        assert objects.index("test_key_1") < objects.index("test_key_2")

        # Clean up
        await storage.delete_object("test_key_1")
        await storage.delete_object("test_key_2")
        await storage.delete_object("test_key_3")

    finally:
        await storage.close()


@requires_redis
@pytest.mark.asyncio
async def test_redis_hierarchical_timestamp_ordering():
    """Test hierarchical key timestamp ordering (requires Redis running)."""
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

        # List batch objects - should be ordered by creation time
        objects, _ = await storage.list_objects("batch", "/")

        # Should be ordered by creation timestamp
        assert len(objects) == 3
        assert objects.index("batch/job_003") < objects.index("batch/job_001")
        assert objects.index("batch/job_001") < objects.index("batch/job_002")

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
        # Create test objects
        for i in range(10):
            await storage.put_object(f"test_key_{i:02d}", f"data_{i}".encode())

        # Test token-based pagination
        page1, token1 = await storage.list_objects(limit=3)
        page2, token2 = await storage.list_objects(limit=3, continuation_token=token1)
        page3, token3 = await storage.list_objects(limit=3, continuation_token=token2)

        # Should have 3 items each (except maybe last page)
        assert len(page1) == 3
        assert len(page2) == 3
        assert len(page3) == 3

        # Tokens should be present for first two pages
        assert token1 is not None
        assert token2 is not None
        # Last page might have more items or not

        # Should be in timestamp order and not overlap
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
        page2, token2 = await storage.list_objects(
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
