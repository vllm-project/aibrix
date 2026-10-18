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

"""
LocalStorage specific tests.

Tests functionality specific to the local filesystem storage implementation.
"""

import json
import os
import shutil
import tempfile
from datetime import datetime
from pathlib import Path
from unittest.mock import AsyncMock

import pytest

from aibrix.batch.job_entity.batch_job import BatchJob, BatchJobSpec, BatchJobState
from aibrix.batch.state.job_store import JobStore
from aibrix.batch.storage import batch_metastore
from aibrix.storage import LocalStorage
from aibrix.storage.factory import create_storage_from_env
from aibrix.storage.redis_upgrade import maybe_upgrade_storage


def _write_pair(storage, key, payload):
    stamp = "2026-01-01T00:00:01+00:00"
    if isinstance(payload, BatchJob):
        stamp = payload.status.created_at.isoformat()
        payload = payload.model_dump_json(by_alias=True, exclude_none=True).encode()
    path = storage.base_path / key
    sidecar = storage.base_path / f"{key}.metadata"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(payload)
    data = {"created_at": stamp, "content_length": len(payload)}
    sidecar.write_text(json.dumps(data))
    return path, sidecar


def _batch_job(job_id, day=1, finished=False):
    spec = BatchJobSpec(input_file_id="input-1", endpoint="/v1/chat/completions")
    job = BatchJob.new_from_spec(job_id, "default", spec)
    job.status.job_id = job_id
    stamp = datetime.fromisoformat(f"2026-01-0{day}T00:00:01+00:00")
    job.status.created_at = job.metadata.creation_timestamp = stamp
    if finished:
        job.status.state, job.status.finalized_at = BatchJobState.FINALIZED, stamp
    return job


def _snapshot(storage):
    files = (p for p in storage.base_path.rglob("*") if p.is_file())
    return {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in files}


async def _assert_upgrade(storage, job, outcome="migrated"):
    before = _snapshot(storage)
    key = f"batchjob:{job.job_id}"
    old_metadata = storage.base_path / f"{key}.metadata"
    new_metadata = storage._get_metadata_path(f"batchjob/{job.job_id}")
    sidecar = (old_metadata if old_metadata.exists() else new_metadata).read_bytes()
    if outcome == "conflict":
        with pytest.raises(RuntimeError, match="conflicting records"):
            await maybe_upgrade_storage(storage)
    else:
        await maybe_upgrade_storage(storage)
    if outcome in ("conflict", "skipped"):
        assert _snapshot(storage) == before
    else:
        assert await batch_metastore.get_batch_job(job.job_id) == job
        assert not (storage.base_path / key).exists() and not old_metadata.exists()
        assert new_metadata.read_bytes() == sidecar
        payload = job.model_dump_json(by_alias=True, exclude_none=True).encode()
        assert await storage.get_object(f"batchjob/{job.job_id}") == payload
    after = _snapshot(storage)
    for path, data in before.items():
        if path.parent == new_metadata.parent:
            assert after[path] == data


@pytest.mark.asyncio
class TestLocalBatchMetastoreUpgrade:
    @pytest.fixture
    def migration(self, local_storage, monkeypatch):
        monkeypatch.setattr(batch_metastore, "p_metastore", local_storage)
        job = _batch_job("old-job")
        _write_pair(local_storage, "batchjob:old-job", job)
        return local_storage, job

    @pytest.mark.parametrize(
        "state",
        [
            (True, False, False, False, False, "migrated"),
            (True, True, False, False, True, "resumed"),
            (True, True, False, False, False, "conflict"),
            (True, True, True, True, True, "converged"),
            (True, False, True, True, False, "conflict"),
            (True, True, True, True, False, "conflict"),
            (True, True, True, False, True, "conflict"),
            (True, False, True, False, False, "conflict"),
            (True, True, True, False, False, "conflict"),
            (False, True, True, True, True, "resumed"),
            (False, False, True, True, False, "conflict"),
            (False, True, True, True, False, "conflict"),
            (False, False, False, False, False, "skipped"),
            (False, True, False, False, True, "skipped"),
            (False, True, False, False, False, "skipped"),
        ],
    )
    async def test_job_state_table(self, migration, caplog, state):
        storage, legacy_job = migration
        # LP: legacy payload; DP: destination sidecar; D: destination payload.
        lp, dp, d, d_equal, dp_equal, outcome = state
        old = storage.base_path / "batchjob:old-job"
        old_meta = storage.base_path / "batchjob:old-job.metadata"
        new = storage._get_full_path("batchjob/old-job")
        new_meta = storage._get_metadata_path("batchjob/old-job")
        new.parent.mkdir()
        for path, present, equal, source in (
            (new, d, d_equal, old),
            (new_meta, dp, dp_equal, old_meta),
        ):
            if present:
                path.write_bytes(source.read_bytes() if equal else b"different")
        if not lp:
            old.unlink()
        await _assert_upgrade(storage, legacy_job, outcome)
        if outcome == "skipped":
            assert sum(r.levelname == "WARNING" for r in caplog.records) == 1

    @pytest.mark.parametrize(
        "boundary", ["copyfile", "replace", "unlink", "watermark", "orphan", "none"]
    )
    async def test_retry_and_cleanup(self, migration, monkeypatch, caplog, boundary):
        storage, legacy_job = migration
        artifact = boundary if boundary in ("orphan", "none") else "pair"
        watermark = batch_metastore.OLDEST_UNFINISHED_JOB_CREATED_AT_KEY
        current_jobs = [_batch_job(f"finished-{day}", day, True) for day in (3, 2)]
        for job in current_jobs:
            _write_pair(storage, f"batchjob/{job.job_id}", job)
        await storage.put_object("batchstatus_copies:job-2/w1", b"current")
        await storage.put_object("file-123", b"{}", content_type="application/json")
        for key in ("other:key", "nested/batchjob:leave"):
            _write_pair(storage, key, b"unrelated")
        protected = {
            p: data
            for p, data in _snapshot(storage).items()
            if p.parent != storage.base_path or not p.name.startswith("batchjob:")
        }
        legacy_paths = []
        for key in (watermark, "batchstatus_copies:old-job:worker-1"):
            legacy_paths.extend(_write_pair(storage, key, b"legacy"))
        await storage.put_object(watermark, b"2026-01-03T00:00:01+00:00")
        if artifact != "pair":
            (storage.base_path / "batchjob:old-job").unlink()
        if artifact == "none":
            (storage.base_path / "batchjob:old-job.metadata").unlink()
        monkeypatch.setattr(JobStore, "DEFAULT_JOB_PAGE_LIMIT", 1)
        store = JobStore()
        assert await store._list_active_jobs() == []
        before = _snapshot(storage)
        if artifact == "pair":
            with monkeypatch.context() as patch:
                if boundary == "watermark":
                    delete = AsyncMock(side_effect=PermissionError("interrupted"))
                    patch.setattr(storage, "delete_object", delete)
                else:
                    owners = dict(copyfile=shutil, replace=os, unlink=Path)
                    owner = owners[boundary]
                    suffix = "" if boundary == "replace" else ".metadata"
                    source = "batchjob:old-job" + suffix
                    operation = getattr(owner, boundary)

                    def interrupt(path, *args, **kwargs):
                        operation(path, *args, **kwargs)
                        if Path(path) == storage.base_path / source:
                            raise OSError("interrupted")

                    patch.setattr(owner, boundary, interrupt)
                with pytest.raises(OSError, match="interrupted"):
                    await maybe_upgrade_storage(storage)
            if boundary == "watermark":
                delete.assert_awaited_once_with(watermark)
                assert _snapshot(storage) == before
            else:
                assert not await storage.object_exists(watermark)
                assert not storage._get_metadata_path(watermark).exists()
        if artifact == "pair":
            await _assert_upgrade(storage, legacy_job)
        else:
            await maybe_upgrade_storage(storage)
        after = _snapshot(storage)
        caplog.clear()
        await maybe_upgrade_storage(storage)
        assert _snapshot(storage) == after
        if artifact != "orphan":
            assert not caplog.records
        assert all(not p.exists() for p in legacy_paths)
        assert all(after[p] == data for p, data in protected.items())
        expected = [legacy_job] if artifact == "pair" else []
        assert await store._list_active_jobs() == expected
        assert await batch_metastore.list_batch_jobs() == current_jobs + expected
        if artifact in ("none", "orphan"):
            assert await storage.get_object(watermark) == b"2026-01-03T00:00:01+00:00"
        else:
            assert not await storage.object_exists(watermark)
            assert not storage._get_metadata_path(watermark).exists()

    async def test_missing_metastore_root_is_a_no_op(self, migration):
        storage, _ = migration
        shutil.rmtree(storage.base_path)
        await maybe_upgrade_storage(storage)
        assert not storage.base_path.exists()

    async def test_other_local_roots_are_left_alone(self, migration, tmp_path):
        other = LocalStorage(str(tmp_path / "files"))
        _write_pair(other, "batchjob:old-job", b"legacy")
        _write_pair(other, "batchstatus_copies:old-job:worker-1", b"legacy")
        before = _snapshot(other)
        await maybe_upgrade_storage(other)
        assert _snapshot(other) == before

    async def test_all_conflicts_and_deleted_job(self, migration):
        storage, legacy_job = migration
        current_job = _batch_job("old-job", 2, True)
        _write_pair(storage, "batchjob/old-job", current_job)
        _write_pair(storage, "batchjob:another-job", b"legacy")
        await storage.put_object("batchjob/another-job", b"conflict")
        before = _snapshot(storage)
        job = _batch_job("valid-job")
        paths = _write_pair(storage, "batchjob:valid-job", job)
        with pytest.raises(RuntimeError, match="conflicting records") as error:
            await maybe_upgrade_storage(storage)
        message = str(error.value)
        assert "batchjob:old-job" in message and "batchjob:another-job" in message
        assert "resolve them manually" in message and str(storage.base_path) in message
        assert all(_snapshot(storage)[p] == data for p, data in before.items())
        assert await batch_metastore.get_batch_job(job.job_id) == job
        assert all(not path.exists() for path in paths)
        for path in before:
            if path.name.startswith("batchjob:"):
                path.unlink()
        await batch_metastore.delete_batch_job("another-job")
        await maybe_upgrade_storage(storage)
        assert await batch_metastore.get_batch_job("old-job") == current_job
        await batch_metastore.delete_batch_job("old-job")
        await maybe_upgrade_storage(storage)
        assert await batch_metastore.get_batch_job("old-job") is None
        assert await batch_metastore.list_batch_jobs() == [job]


class TestLocalStorage:
    """Test LocalStorage specific functionality."""

    @pytest.mark.asyncio
    async def test_local_storage_initialization(self):
        """Test LocalStorage initialization with different base paths."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            # Test with explicit base path
            storage = LocalStorage(base_path=tmp_dir)
            assert storage.base_path == Path(tmp_dir)
            assert storage.base_path.exists()

    @pytest.mark.asyncio
    async def test_environment_variable_override(self):
        """Test that STORAGE_LOCAL_PATH environment variable is respected."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            # Set environment variable
            original_value = os.environ.get("STORAGE_LOCAL_PATH")
            os.environ["STORAGE_LOCAL_PATH"] = tmp_dir

            try:
                # Create storage without base_path - should use env var
                storage = LocalStorage()
                assert str(storage.base_path) == tmp_dir
            finally:
                # Restore original environment
                if original_value is not None:
                    os.environ["STORAGE_LOCAL_PATH"] = original_value
                else:
                    os.environ.pop("STORAGE_LOCAL_PATH", None)

    @pytest.mark.asyncio
    async def test_factory_uses_explicit_local_storage_type(self, monkeypatch):
        with tempfile.TemporaryDirectory() as tmp_dir:
            monkeypatch.setenv("STORAGE_TYPE", "local")
            monkeypatch.setenv("STORAGE_LOCAL_PATH", tmp_dir)
            storage = create_storage_from_env()
            assert isinstance(storage, LocalStorage)
            assert str(storage.base_path) == tmp_dir

    @pytest.mark.asyncio
    async def test_directory_creation(self, local_storage: LocalStorage):
        """Test that directories are created automatically."""
        key = "deep/nested/path/file.txt"
        content = "Nested file content"

        # Store file in nested path
        await local_storage.put_object(key, content)

        # Verify file exists
        assert await local_storage.object_exists(key)

        # Verify directory structure was created
        full_path = local_storage._get_full_path(key)
        assert full_path.parent.exists()
        assert full_path.parent.is_dir()

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_text_content_round_trips_without_newline_translation(
        self, local_storage: LocalStorage
    ):
        """Text objects must come back byte for byte, newlines included.

        The text branch of _write_file opens the temp file in text mode, so
        without newline="" Python rewrites every "\\n" to os.linesep and a
        stored object comes back altered on Windows. This can only fail there:
        os.linesep is "\\n" on Linux, so the translation is invisible in CI.
        """
        key = "test/newlines.txt"
        content = "first\nsecond\nthird"
        await local_storage.put_object(key, content)
        retrieved = await local_storage.get_object(key)
        assert retrieved == content.encode("utf-8")

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_file_permissions(self, local_storage: LocalStorage):
        """Test that files are created with appropriate permissions."""
        key = "test/permissions.txt"
        content = "Test file permissions"

        # Store file
        await local_storage.put_object(key, content)

        # Check file exists and is readable
        full_path = local_storage._get_full_path(key)
        assert full_path.exists()
        assert full_path.is_file()
        assert os.access(full_path, os.R_OK)
        assert os.access(full_path, os.W_OK)

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_with_delimiter(self, local_storage: LocalStorage):
        """Test listing with delimiter for directory-like behavior."""
        # Create test structure
        test_files = [
            "dir1/file1.txt",
            "dir1/file2.txt",
            "dir1/subdir/file3.txt",
            "dir2/file4.txt",
        ]

        for key in test_files:
            await local_storage.put_object(key, f"content of {key}")

        # List with delimiter - should show directories
        result, _ = await local_storage.list_objects("", "/")

        # Should include files at root and directories
        dir_entries = [item for item in result if item.endswith("/")]
        assert "dir1/" in dir_entries
        assert "dir2/" in dir_entries

        # List specific directory
        dir1_contents, _ = await local_storage.list_objects("dir1/", "/")
        file_entries = [item for item in dir1_contents if not item.endswith("/")]

        assert "dir1/file1.txt" in file_entries
        assert "dir1/file2.txt" in file_entries

        # Cleanup
        for key in test_files:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_objects_supports_after_key(self, local_storage: LocalStorage):
        test_files = [
            "after/file1.txt",
            "after/file2.txt",
            "after/file3.txt",
        ]

        for key in test_files:
            await local_storage.put_object(key, f"content of {key}")

        first_page, _ = await local_storage.list_objects("after/", limit=2)
        second_page, _ = await local_storage.list_objects(
            "after/",
            limit=2,
            after_key=first_page[-1],
        )

        assert first_page == ["after/file3.txt", "after/file2.txt"]
        assert second_page == ["after/file1.txt"]

        for key in test_files:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_with_flat_string_prefix(self, local_storage: LocalStorage):
        """list_objects matches a STRING prefix, not just a directory path.

        Flat root-level keys like ``flatkey:<id>`` (no '/') must be found by a
        partial prefix such as ``flatkey:``. The old directory-descent
        implementation returned nothing for any non-directory prefix.
        """
        await local_storage.put_object("flatkey:abc", b"a")
        await local_storage.put_object("flatkey:def", b"b")
        await local_storage.put_object("other:zzz", b"c")

        keys, _ = await local_storage.list_objects(prefix="flatkey:")
        assert keys == ["flatkey:def", "flatkey:abc"]

        # A delimiter must not break flat-prefix matching.
        keys2, _ = await local_storage.list_objects(prefix="flatkey:", delimiter=":")
        assert keys2 == ["flatkey:def", "flatkey:abc"]

        for key in ("flatkey:abc", "flatkey:def", "other:zzz"):
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_prefix_orders_by_created_at_desc(
        self, local_storage: LocalStorage
    ):
        keys = ["batchjob:job-b", "batchjob:job-a", "batchjob:job-c"]
        created_at_by_key = {
            "batchjob:job-b": "2026-01-01T00:00:01+00:00",
            "batchjob:job-a": "2026-01-01T00:00:02+00:00",
            "batchjob:job-c": "2026-01-01T00:00:03+00:00",
        }

        for key in keys:
            await local_storage.put_object(key, key)
            metadata_path = local_storage._get_metadata_path(key)
            metadata = local_storage._read_json_file(metadata_path)
            metadata["created_at"] = created_at_by_key[key]
            local_storage._write_json_file(metadata_path, metadata)

        first_page, _ = await local_storage.list_objects(prefix="batchjob:", limit=2)
        second_page, _ = await local_storage.list_objects(
            prefix="batchjob:",
            limit=2,
            after_key=first_page[-1],
        )

        assert first_page == ["batchjob:job-c", "batchjob:job-a"]
        assert second_page == ["batchjob:job-b"]

        for key in keys:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_hierarchical_keys_with_colon_parent_are_supported(
        self, local_storage: LocalStorage
    ):
        key = "batchstatus_copies:job-1/cluster-a"

        await local_storage.put_object(key, b"payload")

        objects, _ = await local_storage.list_objects(
            prefix="batchstatus_copies:job-1/",
            delimiter="/",
        )
        assert objects == [key]
        assert await local_storage.object_exists(key)

        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_recursive_list_preserves_nested_subfolders(
        self, local_storage: LocalStorage
    ):
        key = "batchstatus_copies:job-1/cluster-a/default/workload-1"

        await local_storage.put_object(key, b"payload")

        objects, _ = await local_storage.list_objects(
            prefix="batchstatus_copies:job-1/",
        )
        assert objects == [key]

        objects, _ = await local_storage.list_objects(
            prefix="batchstatus_copies:job-1/cluster-a/",
        )
        assert objects == [key]

        objects, _ = await local_storage.list_objects(
            prefix="batchstatus_copies:job-1/cluster-a/default/",
        )
        assert objects == [key]

        assert await local_storage.object_exists(key)

        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_concurrent_operations(self, local_storage: LocalStorage):
        """Test concurrent read/write operations."""
        import asyncio

        async def write_file(key: str, content: str):
            await local_storage.put_object(key, content)

        async def read_file(key: str) -> str:
            data = await local_storage.get_object(key)
            return data.decode("utf-8")

        # Write multiple files concurrently
        keys = [f"concurrent/file_{i}.txt" for i in range(10)]
        write_tasks = [write_file(key, f"content_{i}") for i, key in enumerate(keys)]
        await asyncio.gather(*write_tasks)

        # Read multiple files concurrently
        read_tasks = [read_file(key) for key in keys]
        results = await asyncio.gather(*read_tasks)

        # Verify results
        for i, result in enumerate(results):
            assert result == f"content_{i}"

        # Cleanup
        for key in keys:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_large_file_handling(self, local_storage: LocalStorage):
        """Test handling of larger files."""
        key = "test/large_file.bin"
        # Create 1MB of data
        chunk_size = 1024
        num_chunks = 1024
        content = b"A" * chunk_size * num_chunks

        # Store large file
        await local_storage.put_object(key, content)

        # Verify size
        size = await local_storage.get_object_size(key)
        assert size == len(content)

        # Test range reads on large file
        start_chunk = await local_storage.get_object(key, 0, chunk_size - 1)
        assert len(start_chunk) == chunk_size
        assert start_chunk == b"A" * chunk_size

        # Test end chunk
        end_start = len(content) - chunk_size
        end_chunk = await local_storage.get_object(key, end_start, None)
        assert len(end_chunk) == chunk_size
        assert end_chunk == b"A" * chunk_size

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_path_traversal_protection(self, local_storage: LocalStorage):
        """Test that path traversal attempts are properly sanitized and denied."""
        # These dangerous keys should be sanitized to prevent path traversal
        dangerous_keys = [
            "../etc/passwd",
            "../../sensitive/file.txt",
            "normal/../../../etc/hosts",
            "../../../root/.ssh/id_rsa",
            "..\\..\\windows\\system32\\config\\sam",  # Windows-style traversal
            "....//....//etc/shadow",  # Double-dot traversal
            "/etc/passwd",  # Absolute path
            "~/../../etc/passwd",  # Home directory traversal
        ]

        expected_sanitized_keys = [
            "etc/passwd",  # "../etc/passwd" -> "etc/passwd"
            "sensitive/file.txt",  # "../../sensitive/file.txt" -> "sensitive/file.txt"
            "normal/etc/hosts",  # "normal/../../../etc/hosts" -> "normal/etc/hosts"
            "root/.ssh/id_rsa",  # "../../../root/.ssh/id_rsa" -> "root/.ssh/id_rsa"
            "windows/system32/config/sam",  # Windows path sanitized
            "etc/shadow",  # "....//....//etc/shadow" -> "etc/shadow"
            "etc/passwd",  # "/etc/passwd" -> "etc/passwd"
            "~/etc/passwd",  # "~/../../etc/passwd" -> "~/etc/passwd"
        ]

        for i, key in enumerate(dangerous_keys):
            content = f"content for {key}"
            await local_storage.put_object(key, content)

            # Verify the key was sanitized
            full_path = local_storage._get_full_path(key)

            # Should be stored safely within base directory
            assert (
                local_storage.base_path in full_path.parents
                or full_path == local_storage.base_path
            ), (
                f"Path {full_path} should be within base directory {local_storage.base_path}"
            )

            # Verify the path doesn't contain traversal patterns
            relative_path = full_path.relative_to(local_storage.base_path)
            assert ".." not in str(relative_path), (
                f"Sanitized path should not contain '..' but got: {relative_path}"
            )

            # Should be retrievable with the original key
            result = await local_storage.get_object(key)
            assert result.decode("utf-8") == content

            # Verify the actual stored filename matches expected sanitized version
            expected_sanitized = expected_sanitized_keys[i]
            assert str(relative_path).startswith(
                expected_sanitized.replace("/", os.sep)
            ), (
                f"Expected sanitized key '{expected_sanitized}' but got '{relative_path}'"
            )

            # Cleanup
            await local_storage.delete_object(key)
