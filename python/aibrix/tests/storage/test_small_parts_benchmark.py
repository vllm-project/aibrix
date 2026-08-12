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

import os
import time
import tracemalloc
from collections import defaultdict
from types import MethodType

import pytest

from aibrix.storage.base import BaseStorage
from aibrix.storage.base2 import BaseStorage2
from aibrix.storage.tos import TOSStorage

_RUN_BENCHMARK_ENV = "AIBRIX_RUN_STORAGE_BENCHMARK"
_LINE_COUNT = 1000
_LINE_SIZE_BYTES = 1024


def _emit_pytest_log(pytestconfig: pytest.Config, message: str) -> None:
    terminal_reporter = pytestconfig.pluginmanager.get_plugin("terminalreporter")
    if terminal_reporter is not None:
        terminal_reporter.write_line(message)


def _make_benchmark_line(index: int) -> bytes:
    prefix = f"line-{index:05d}:"
    payload_size = _LINE_SIZE_BYTES - len(prefix) - 1
    if payload_size <= 0:
        raise ValueError("Benchmark line prefix exceeds configured line size")
    return (prefix + ("x" * payload_size) + "\n").encode("utf-8")


class TestSmallPartsBenchmark:
    async def _run_small_parts_benchmark(
        self,
        tos_storage: TOSStorage,
        monkeypatch: pytest.MonkeyPatch,
        pytestconfig: pytest.Config,
        *,
        use_base2: bool,
    ) -> None:
        if use_base2:
            monkeypatch.setattr(
                tos_storage,
                "complete_multipart_upload",
                MethodType(BaseStorage2.complete_multipart_upload, tos_storage),
            )
            monkeypatch.setattr(
                tos_storage,
                "_flush_native_aggregate_part",
                MethodType(BaseStorage2._flush_native_aggregate_part, tos_storage),
                raising=False,
            )
            monkeypatch.setattr(
                tos_storage,
                "_iter_prefetched_staged_parts",
                MethodType(BaseStorage2._iter_prefetched_staged_parts, tos_storage),
                raising=False,
            )
            monkeypatch.setattr(
                tos_storage,
                "abort_multipart_upload",
                MethodType(BaseStorage2.abort_multipart_upload, tos_storage),
            )
        else:
            monkeypatch.setattr(
                tos_storage,
                "upload_part",
                MethodType(BaseStorage.upload_part, tos_storage),
            )
            monkeypatch.setattr(
                tos_storage,
                "complete_multipart_upload",
                MethodType(BaseStorage.complete_multipart_upload, tos_storage),
            )
            monkeypatch.setattr(
                tos_storage,
                "abort_multipart_upload",
                MethodType(BaseStorage.abort_multipart_upload, tos_storage),
            )

        benchmark_target = "base2" if use_base2 else "legacy"

        if os.getenv(_RUN_BENCHMARK_ENV) != "1":
            pytest.skip(f"Set {_RUN_BENCHMARK_ENV}=1 to run the storage benchmark")

        key = f"benchmark/small-parts-{benchmark_target}-5000-lines.txt"
        upload_id = await tos_storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={
                "benchmark": "small-parts",
                "line_count": str(_LINE_COUNT),
                "line_size_bytes": str(_LINE_SIZE_BYTES),
                "benchmark_target": benchmark_target,
            },
            small_parts=True,
        )
        multipart_prefix = tos_storage._multipart_upload_key(upload_id, "")
        metadata_key = tos_storage._multipart_upload_key(upload_id)

        profile_enabled = False
        profile_stats: defaultdict[str, float] = defaultdict(float)
        profile_counts: defaultdict[str, int] = defaultdict(int)

        original_get_object = tos_storage.get_object
        original_put_object = tos_storage.put_object
        original_list_objects = tos_storage.list_objects
        original_delete_object = tos_storage.delete_object
        original_delete_objects = tos_storage.delete_objects
        original_abort_multipart_upload = tos_storage.abort_multipart_upload
        original_native_create_multipart_upload = (
            tos_storage._native_create_multipart_upload
        )
        original_native_upload_part = tos_storage._native_upload_part
        original_native_complete_multipart_upload = (
            tos_storage._native_complete_multipart_upload
        )
        original_native_abort_multipart_upload = (
            tos_storage._native_abort_multipart_upload
        )
        original_iter_prefetched_staged_parts = getattr(
            tos_storage, "_iter_prefetched_staged_parts", None
        )

        async def profiled_get_object(
            obj_key: str, range_start: int | None = None, range_end: int | None = None
        ) -> bytes:
            started = time.perf_counter()
            result = await original_get_object(obj_key, range_start, range_end)
            elapsed = time.perf_counter() - started
            if profile_enabled:
                if obj_key == metadata_key:
                    profile_stats["metadata_get_elapsed_seconds"] += elapsed
                    profile_counts["metadata_get_count"] += 1
                elif obj_key.startswith(multipart_prefix):
                    profile_stats["part_get_elapsed_seconds"] += elapsed
                    profile_counts["part_get_count"] += 1
                    profile_stats["part_get_bytes"] += len(result)
                else:
                    profile_stats["other_get_elapsed_seconds"] += elapsed
                    profile_counts["other_get_count"] += 1
            return result

        async def profiled_put_object(
            obj_key: str,
            data,
            content_type: str | None = None,
            metadata: dict[str, str] | None = None,
            options=None,
        ) -> bool:
            started = time.perf_counter()
            result = await original_put_object(
                obj_key, data, content_type, metadata, options
            )
            elapsed = time.perf_counter() - started
            if profile_enabled:
                if obj_key == key:
                    profile_stats["final_put_elapsed_seconds"] += elapsed
                    profile_counts["final_put_count"] += 1
                    if hasattr(data, "seek") and hasattr(data, "tell"):
                        current_pos = data.tell()
                        data.seek(0, 2)
                        profile_stats["final_put_bytes"] += data.tell()
                        data.seek(current_pos)
                else:
                    profile_stats["other_put_elapsed_seconds"] += elapsed
                    profile_counts["other_put_count"] += 1
            return result

        async def profiled_native_create_multipart_upload(
            obj_key: str,
            content_type: str | None = None,
            metadata: dict[str, str] | None = None,
        ) -> str:
            started = time.perf_counter()
            result = await original_native_create_multipart_upload(
                obj_key, content_type, metadata
            )
            elapsed = time.perf_counter() - started
            if profile_enabled:
                profile_stats["native_create_elapsed_seconds"] += elapsed
                profile_counts["native_create_count"] += 1
            return result

        async def profiled_native_upload_part(
            obj_key: str,
            target_upload_id: str,
            part_number: int,
            data,
        ) -> str:
            started = time.perf_counter()
            result = await original_native_upload_part(
                obj_key, target_upload_id, part_number, data
            )
            elapsed = time.perf_counter() - started
            if profile_enabled and target_upload_id != upload_id:
                profile_stats["native_part_upload_elapsed_seconds"] += elapsed
                profile_counts["native_part_upload_count"] += 1
                if hasattr(data, "seek") and hasattr(data, "tell"):
                    current_pos = data.tell()
                    data.seek(0, 2)
                    profile_stats["native_part_upload_bytes"] += data.tell()
                    data.seek(current_pos)
            return result

        async def profiled_native_complete_multipart_upload(
            obj_key: str,
            target_upload_id: str,
            completed_parts: list[dict[str, str | int]],
        ) -> None:
            started = time.perf_counter()
            await original_native_complete_multipart_upload(
                obj_key, target_upload_id, completed_parts
            )
            elapsed = time.perf_counter() - started
            if profile_enabled and target_upload_id != upload_id:
                profile_stats["native_complete_elapsed_seconds"] += elapsed
                profile_counts["native_complete_count"] += 1

        async def profiled_native_abort_multipart_upload(
            obj_key: str, target_upload_id: str
        ) -> None:
            started = time.perf_counter()
            await original_native_abort_multipart_upload(obj_key, target_upload_id)
            elapsed = time.perf_counter() - started
            if profile_enabled and target_upload_id != upload_id:
                profile_stats["native_abort_elapsed_seconds"] += elapsed
                profile_counts["native_abort_count"] += 1

        async def profiled_abort_multipart_upload(
            obj_key: str, target_upload_id: str
        ) -> None:
            started = time.perf_counter()
            await original_abort_multipart_upload(obj_key, target_upload_id)
            elapsed = time.perf_counter() - started
            if profile_enabled and target_upload_id == upload_id:
                profile_stats["cleanup_total_elapsed_seconds"] += elapsed
                profile_counts["cleanup_total_count"] += 1

        async def profiled_delete_objects(keys: list[str]) -> None:
            started = time.perf_counter()
            await original_delete_objects(keys)
            elapsed = time.perf_counter() - started
            if profile_enabled:
                profile_stats["cleanup_bulk_delete_elapsed_seconds"] += elapsed
                profile_counts["cleanup_bulk_delete_count"] += 1
                profile_counts["cleanup_bulk_deleted_objects"] += len(keys)

        async def profiled_list_objects(
            prefix: str = "",
            delimiter: str | None = None,
            limit: int | None = None,
            continuation_token: str | None = None,
            after_key: str | None = None,
        ):
            started = time.perf_counter()
            result = await original_list_objects(
                prefix, delimiter, limit, continuation_token, after_key
            )
            elapsed = time.perf_counter() - started
            if profile_enabled and prefix == multipart_prefix:
                profile_stats["cleanup_list_elapsed_seconds"] += elapsed
                profile_counts["cleanup_list_count"] += 1
                profile_counts["cleanup_listed_objects"] += len(result[0])
            return result

        async def profiled_delete_object(obj_key: str) -> None:
            started = time.perf_counter()
            await original_delete_object(obj_key)
            elapsed = time.perf_counter() - started
            if profile_enabled and obj_key.startswith(multipart_prefix):
                profile_stats["cleanup_delete_elapsed_seconds"] += elapsed
                profile_counts["cleanup_delete_count"] += 1

        if original_iter_prefetched_staged_parts is not None:

            async def profiled_iter_prefetched_staged_parts(
                current_upload_id: str,
                current_parts: list[dict[str, str | int]],
                current_staged_part_keys: list[str],
            ):
                started = time.perf_counter()
                async for item in original_iter_prefetched_staged_parts(
                    current_upload_id, current_parts, current_staged_part_keys
                ):
                    yield item
                elapsed = time.perf_counter() - started
                if profile_enabled:
                    profile_stats["part_fetch_wall_clock_seconds"] += elapsed
                    profile_counts["part_fetch_window_count"] += 1

        monkeypatch.setattr(tos_storage, "get_object", profiled_get_object)
        monkeypatch.setattr(tos_storage, "put_object", profiled_put_object)
        monkeypatch.setattr(tos_storage, "list_objects", profiled_list_objects)
        monkeypatch.setattr(tos_storage, "delete_object", profiled_delete_object)
        monkeypatch.setattr(tos_storage, "delete_objects", profiled_delete_objects)
        monkeypatch.setattr(
            tos_storage, "abort_multipart_upload", profiled_abort_multipart_upload
        )
        monkeypatch.setattr(
            tos_storage,
            "_native_create_multipart_upload",
            profiled_native_create_multipart_upload,
        )
        monkeypatch.setattr(
            tos_storage,
            "_native_upload_part",
            profiled_native_upload_part,
        )
        monkeypatch.setattr(
            tos_storage,
            "_native_complete_multipart_upload",
            profiled_native_complete_multipart_upload,
        )
        monkeypatch.setattr(
            tos_storage,
            "_native_abort_multipart_upload",
            profiled_native_abort_multipart_upload,
        )
        if original_iter_prefetched_staged_parts is not None:
            monkeypatch.setattr(
                tos_storage,
                "_iter_prefetched_staged_parts",
                profiled_iter_prefetched_staged_parts,
            )

        parts: list[dict[str, str | int]] = []
        lines = [_make_benchmark_line(index) for index in range(_LINE_COUNT)]

        upload_started = time.perf_counter()
        for part_number, line in enumerate(lines, start=1):
            etag = await tos_storage.upload_part(key, upload_id, part_number, line)
            parts.append({"part_number": part_number, "etag": etag})
        upload_elapsed = time.perf_counter() - upload_started

        complete_started = time.perf_counter()
        tracemalloc.start()
        profile_enabled = True
        await tos_storage.complete_multipart_upload(key, upload_id, parts)
        profile_enabled = False
        current_traced_bytes, peak_traced_bytes = tracemalloc.get_traced_memory()
        tracemalloc.stop()
        complete_elapsed = time.perf_counter() - complete_started

        total_elapsed = upload_elapsed + complete_elapsed
        result = await tos_storage.get_object(key)
        expected = b"".join(lines)

        assert result == expected
        used_native_reassembly = profile_counts["native_create_count"] > 0
        used_legacy_reassembly = profile_counts["final_put_count"] > 0

        assert used_native_reassembly or used_legacy_reassembly
        if used_native_reassembly:
            assert profile_counts["native_create_count"] == 1
            assert profile_counts["native_complete_count"] == 1
            assert profile_counts["native_part_upload_count"] >= 1
            assert profile_counts["final_put_count"] == 0
        if used_legacy_reassembly:
            assert profile_counts["final_put_count"] == 1
            assert profile_counts["native_create_count"] == 0
            assert profile_counts["native_complete_count"] == 0
            assert profile_counts["native_part_upload_count"] == 0
            assert profile_counts["cleanup_total_count"] == 1

        if used_native_reassembly:
            part_fetch_wall_clock_seconds = profile_stats[
                "part_fetch_wall_clock_seconds"
            ]
        else:
            # Legacy path reads staged parts sequentially, so sum latency is also wall clock.
            part_fetch_wall_clock_seconds = profile_stats["part_get_elapsed_seconds"]

        complete_accounted_wall_clock_seconds = (
            profile_stats["metadata_get_elapsed_seconds"]
            + part_fetch_wall_clock_seconds
            + profile_stats["final_put_elapsed_seconds"]
            + profile_stats["native_create_elapsed_seconds"]
            + profile_stats["native_part_upload_elapsed_seconds"]
            + profile_stats["native_complete_elapsed_seconds"]
            + profile_stats["native_abort_elapsed_seconds"]
            + profile_stats["cleanup_total_elapsed_seconds"]
        )
        complete_residual_seconds = (
            complete_elapsed - complete_accounted_wall_clock_seconds
        )
        implementation_name = (
            "bounded-native-reassembly"
            if used_native_reassembly
            else "legacy-put-object"
        )

        _emit_pytest_log(
            pytestconfig,
            (
                "[storage-benchmark] "
                f"backend=tos, benchmark_target={benchmark_target}, "
                f"implementation={implementation_name}, lines={_LINE_COUNT}, "
                f"line_size_bytes={_LINE_SIZE_BYTES}, input_bytes={len(expected)}"
            ),
        )
        _emit_pytest_log(
            pytestconfig,
            (
                "[storage-benchmark] "
                f"upload_elapsed_seconds={upload_elapsed:.6f}, "
                f"complete_elapsed_seconds={complete_elapsed:.6f}, "
                f"total_elapsed_seconds={total_elapsed:.6f}, "
                f"complete_peak_traced_bytes={peak_traced_bytes}, "
                f"complete_current_traced_bytes={current_traced_bytes}"
            ),
        )
        _emit_pytest_log(
            pytestconfig,
            (
                "[storage-benchmark] "
                f"complete_breakdown="
                f"metadata_get={profile_stats['metadata_get_elapsed_seconds']:.6f}s/"
                f"{profile_counts['metadata_get_count']} calls, "
                f"part_get_sum_latency={profile_stats['part_get_elapsed_seconds']:.6f}s/"
                f"{profile_counts['part_get_count']} calls/"
                f"{int(profile_stats['part_get_bytes'])} bytes, "
                f"part_fetch_wall_clock={part_fetch_wall_clock_seconds:.6f}s/"
                f"{profile_counts['part_fetch_window_count']} windows, "
                f"final_put={profile_stats['final_put_elapsed_seconds']:.6f}s/"
                f"{profile_counts['final_put_count']} calls/"
                f"{int(profile_stats['final_put_bytes'])} bytes, "
                f"native_create={profile_stats['native_create_elapsed_seconds']:.6f}s/"
                f"{profile_counts['native_create_count']} calls, "
                f"native_part_upload={profile_stats['native_part_upload_elapsed_seconds']:.6f}s/"
                f"{profile_counts['native_part_upload_count']} calls/"
                f"{int(profile_stats['native_part_upload_bytes'])} bytes, "
                f"native_complete={profile_stats['native_complete_elapsed_seconds']:.6f}s/"
                f"{profile_counts['native_complete_count']} calls, "
                f"native_abort={profile_stats['native_abort_elapsed_seconds']:.6f}s/"
                f"{profile_counts['native_abort_count']} calls"
            ),
        )
        _emit_pytest_log(
            pytestconfig,
            (
                "[storage-benchmark] "
                f"cleanup_breakdown="
                f"list={profile_stats['cleanup_list_elapsed_seconds']:.6f}s/"
                f"{profile_counts['cleanup_list_count']} calls/"
                f"{profile_counts['cleanup_listed_objects']} listed, "
                f"bulk_delete={profile_stats['cleanup_bulk_delete_elapsed_seconds']:.6f}s/"
                f"{profile_counts['cleanup_bulk_delete_count']} calls/"
                f"{profile_counts['cleanup_bulk_deleted_objects']} objects, "
                f"cleanup_total={profile_stats['cleanup_total_elapsed_seconds']:.6f}s/"
                f"{profile_counts['cleanup_total_count']} calls, "
                f"delete={profile_stats['cleanup_delete_elapsed_seconds']:.6f}s/"
                f"{profile_counts['cleanup_delete_count']} deletes, "
                f"complete_residual_seconds={complete_residual_seconds:.6f}s, "
                f"complete_accounted_wall_clock={complete_accounted_wall_clock_seconds:.6f}s"
            ),
        )

        await tos_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_complete_multipart_upload_benchmark_5000_small_parts_legacy(
        self, tos_storage: TOSStorage, monkeypatch: pytest.MonkeyPatch, pytestconfig
    ) -> None:
        await self._run_small_parts_benchmark(
            tos_storage, monkeypatch, pytestconfig, use_base2=False
        )

    @pytest.mark.asyncio
    async def test_complete_multipart_upload_benchmark_5000_small_parts_base2(
        self, tos_storage: TOSStorage, monkeypatch: pytest.MonkeyPatch, pytestconfig
    ) -> None:
        await self._run_small_parts_benchmark(
            tos_storage, monkeypatch, pytestconfig, use_base2=True
        )
