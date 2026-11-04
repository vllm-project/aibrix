# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import threading
import time

import pytest

from aibrix.runtime.model_cache import PreparedCacheIndex
from aibrix.runtime.model_runtime import MockEngineLauncher, ModelRuntime


def eventually(predicate):
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.005)
    assert predicate()


def test_slow_scan_does_not_block_or_duplicate_snapshot_reads():
    entered, release = threading.Event(), threading.Event()
    calls = []

    def scan():
        calls.append(1)
        entered.set()
        assert release.wait(3)
        return ["hf://Org/Model"]

    index = PreparedCacheIndex(scanner=scan, interval=10)
    assert index.get_artifacts() == []
    assert entered.wait(1)
    try:
        for _ in range(20):
            assert index.get_artifacts() == []
        assert len(calls) == 1
    finally:
        release.set()
    eventually(lambda: index.get_artifacts() == ["hf://Org/Model"])


def test_refresh_removes_lost_cache_and_failure_withdraws_hints():
    contents = ["s3://bucket/model"]
    clock = [0.0]
    fail = [False]

    def scan():
        if fail[0]:
            raise OSError("unmounted")
        return list(contents)

    index = PreparedCacheIndex(scanner=scan, interval=10, clock=lambda: clock[0])
    eventually(lambda: index.get_artifacts() == contents)
    contents.clear()
    clock[0] = 11
    eventually(lambda: index.get_artifacts() == [])
    contents.append("tos://bucket/model")
    clock[0] = 22
    eventually(lambda: index.get_artifacts() == contents)
    fail[0] = True
    clock[0] = 33
    index.get_artifacts()
    eventually(lambda: index.get_artifacts() == [])
    fail[0] = False
    clock[0] = 44
    eventually(lambda: index.get_artifacts() == contents)


def test_stuck_refresh_cannot_keep_old_hints_forever():
    clock = [0.0]
    release = threading.Event()
    started = threading.Event()
    calls = []

    def scan():
        calls.append(1)
        if len(calls) > 1:
            started.set()
            assert release.wait(3)
        return ["gcs://bucket/model"]

    index = PreparedCacheIndex(scanner=scan, interval=10, clock=lambda: clock[0])
    eventually(lambda: bool(index.get_artifacts()))
    clock[0] = 11
    index.get_artifacts()
    assert started.wait(1)
    try:
        clock[0] = 21
        assert index.get_artifacts() == []
    finally:
        release.set()


def test_runtime_merges_new_hints_with_legacy_markers(monkeypatch, tmp_path):
    import json

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    marker = tmp_path / ".aibrix/served/old.json"
    marker.parent.mkdir(parents=True)
    marker.write_text(json.dumps({"artifact_url": "hf://Old/Model"}))
    agent = ModelRuntime(MockEngineLauncher())
    agent._cache_index = PreparedCacheIndex(
        scanner=lambda: ["hf://New/Model", "hf://Old/Model"]
    )
    eventually(
        lambda: (
            agent.snapshot()["cached_artifacts"] == ["hf://New/Model", "hf://Old/Model"]
        )
    )


@pytest.mark.asyncio
async def test_app_lifecycle_discovers_before_first_snapshot_and_stops(monkeypatch):
    from fastapi import FastAPI

    from aibrix.app import runtime_lifespan
    from aibrix.runtime import model_runtime

    scanned = threading.Event()

    def scan():
        scanned.set()
        return ["hf://Org/Model"]

    agent = ModelRuntime(MockEngineLauncher(), supervisor_interval_seconds=0.01)
    agent._cache_index = PreparedCacheIndex(scanner=scan)
    monkeypatch.setattr(model_runtime, "_AGENT", agent)
    async with runtime_lifespan(FastAPI()):
        assert scanned.wait(1), "discovery must not wait for a Claim's first snapshot"
        assert agent._supervisor_thread.is_alive()
    assert not agent._supervisor_thread.is_alive()
