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
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from aibrix_kvcache.l2.connectors.shfs import SHFSConnector


class _BytesMR:
    """Stand-in for a MemoryRegion: _put() only calls tobytes()."""

    def __init__(self, data: bytes):
        self._data = data

    def tobytes(self) -> bytes:
        return self._data


def test_concurrent_puts_of_same_key(tmp_path, monkeypatch):
    executor = ThreadPoolExecutor(1)
    conn = SHFSConnector(str(tmp_path / "conn"), executor)
    assert conn.open().is_ok()
    key = bytes.fromhex("00112233445566778899aabbccddeeff")
    payload = os.urandom(4096)

    # Pause the first put between writing its temp file and renaming it,
    # and let a second put of the same key run to completion meanwhile.
    first_written = threading.Event()
    resume_first = threading.Event()
    orig_replace = Path.replace
    paused = []

    def replace(self, target):
        if not paused:
            paused.append(self)
            first_written.set()
            resume_first.wait(5)
        return orig_replace(self, target)

    monkeypatch.setattr(Path, "replace", replace)

    results = {}
    first = threading.Thread(
        target=lambda: results.update(first=conn._put(key, _BytesMR(payload)))
    )
    first.start()
    try:
        assert first_written.wait(5)
        second = conn._put(key, _BytesMR(payload))
    finally:
        resume_first.set()
        first.join(5)
        executor.shutdown()

    assert second.is_ok()
    assert "first" in results
    assert results["first"].is_ok()
    assert conn._key_to_filepath(key).read_bytes() == payload
    assert not list(conn._key_to_filepath(key).parent.glob("*.tmp"))
