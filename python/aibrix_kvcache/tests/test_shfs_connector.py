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
import time
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


def test_concurrent_puts_write_distinct_temp_files(tmp_path, monkeypatch):
    executor = ThreadPoolExecutor(1)
    conn = SHFSConnector(str(tmp_path / "conn"), executor)
    assert conn.open().is_ok()
    key = bytes.fromhex("00112233445566778899aabbccddeeff")
    payload = os.urandom(4096)

    # Pause the first put before its rename, run a second put of the same
    # key, and check that the second put did not write into the first put's
    # temp file (a shared temp file can be renamed while still being written).
    first_written = threading.Event()
    resume_first = threading.Event()
    orig_replace = Path.replace
    temp_files = []

    def replace(self, target):
        temp_files.append((self, self.read_bytes()))
        if len(temp_files) == 1:
            first_written.set()
            resume_first.wait(5)
        return orig_replace(self, target)

    monkeypatch.setattr(Path, "replace", replace)

    first = threading.Thread(target=lambda: conn._put(key, _BytesMR(payload)))
    first.start()
    try:
        assert first_written.wait(5)
        assert conn._put(key, _BytesMR(payload)).is_ok()
        # The first put's temp file is still complete and unrenamed.
        assert temp_files[0][0].read_bytes() == payload
    finally:
        resume_first.set()
        first.join(5)
        executor.shutdown()

    assert len(temp_files) == 2
    assert temp_files[0][0] != temp_files[1][0]


def test_put_removes_its_temp_file_when_rename_fails(tmp_path, monkeypatch):
    executor = ThreadPoolExecutor(1)
    conn = SHFSConnector(str(tmp_path / "conn"), executor)
    assert conn.open().is_ok()
    key = bytes.fromhex("00112233445566778899aabbccddeeff")

    def replace(self, target):
        raise OSError("rename failed")

    monkeypatch.setattr(Path, "replace", replace)
    try:
        status = conn._put(key, _BytesMR(os.urandom(4096)))
    finally:
        executor.shutdown()

    assert not status.is_ok()
    parent = conn._key_to_filepath(key).parent
    assert not list(parent.glob("*.tmp"))
    assert not conn._key_to_filepath(key).exists()


def test_open_removes_only_stale_temp_files(tmp_path):
    executor = ThreadPoolExecutor(1)
    conn = SHFSConnector(str(tmp_path / "conn"), executor)
    filepath = conn._key_to_filepath(bytes.fromhex("0011223344556677"))
    filepath.parent.mkdir(parents=True)
    filepath.write_bytes(b"block")
    stale = filepath.with_name(f"{filepath.name}.stale.tmp")
    stale.write_bytes(b"partial")
    two_hours_ago = time.time() - 2 * 3600
    os.utime(stale, (two_hours_ago, two_hours_ago))
    in_flight = filepath.with_name(f"{filepath.name}.in-flight.tmp")
    in_flight.write_bytes(b"partial")

    try:
        assert conn.open().is_ok()
    finally:
        executor.shutdown()

    assert not stale.exists()
    assert in_flight.exists()
    assert filepath.read_bytes() == b"block"
