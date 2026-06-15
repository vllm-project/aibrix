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
import time

import pytest

import aibrix.runtime.artifact_service as artifact_service_module
from aibrix.runtime.artifact_service import ArtifactDelegationService


@pytest.mark.asyncio
async def test_download_artifact_does_not_block_event_loop(tmp_path, monkeypatch):
    service = ArtifactDelegationService(local_dir=str(tmp_path))

    class FakeDownloader:
        async def download(
            self, source_url: str, local_path: str, credentials=None
        ) -> str:
            os.makedirs(local_path, exist_ok=True)
            await asyncio.to_thread(time.sleep, 0.2)
            with open(os.path.join(local_path, "done"), "w") as f:
                f.write("1")
            return local_path

    monkeypatch.setattr(
        artifact_service_module, "get_downloader", lambda _url: FakeDownloader()
    )

    start = time.perf_counter()
    task = asyncio.create_task(
        service.download_artifact("tos://bucket/prefix/", "adapter", credentials={})
    )

    await asyncio.sleep(0.05)
    assert time.perf_counter() - start < 0.15

    downloaded_path = await task
    assert downloaded_path == str(tmp_path / "adapter")
    assert (tmp_path / "adapter" / "done").exists()
