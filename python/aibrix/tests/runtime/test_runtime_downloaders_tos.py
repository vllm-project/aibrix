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
import sys
import types
from typing import Any

import pytest

from aibrix.runtime.downloaders import TOSArtifactDownloader, get_downloader


def _install_fake_tos(monkeypatch: pytest.MonkeyPatch):
    fake_tos: Any = types.ModuleType("tos")
    fake_tos.last_client = None

    class FakeObj:
        def __init__(self, key: str):
            self.key = key

    class FakeListResp:
        def __init__(self, keys: list[str]):
            self.contents = [FakeObj(key) for key in keys]
            self.is_truncated = False
            self.next_continuation_token = None

    class TosClientV2:
        def __init__(self, **kwargs):
            self.kwargs = kwargs
            self.download_calls = []
            fake_tos.last_client = self

        def list_objects_type2(self, bucket_name: str, **kwargs):
            prefix = kwargs.get("prefix", "")
            keys = [
                prefix,
                f"{prefix}.git/",
                f"{prefix}sub/",
                f"{prefix}a.bin",
                f"{prefix}sub/b.bin",
            ]
            return FakeListResp(keys)

        def download_file(self, bucket: str, key: str, file_path: str, **kwargs):
            os.makedirs(os.path.dirname(file_path), exist_ok=True)
            with open(file_path, "wb") as f:
                f.write(b"data")
            self.download_calls.append((bucket, key, file_path, kwargs))

    fake_tos.TosClientV2 = TosClientV2

    fake_tos_exceptions: Any = types.ModuleType("tos.exceptions")

    class TosClientError(Exception):
        pass

    class TosServerError(Exception):
        pass

    fake_tos_exceptions.TosClientError = TosClientError
    fake_tos_exceptions.TosServerError = TosServerError

    monkeypatch.setitem(sys.modules, "tos", fake_tos)
    monkeypatch.setitem(sys.modules, "tos.exceptions", fake_tos_exceptions)
    return fake_tos


def test_get_downloader_tos_scheme():
    downloader = get_downloader("tos://bucket/path")
    assert isinstance(downloader, TOSArtifactDownloader)


@pytest.mark.asyncio
async def test_tos_downloader_download_directory(
    monkeypatch: pytest.MonkeyPatch, tmp_path
):
    fake_tos = _install_fake_tos(monkeypatch)

    downloader = get_downloader("tos://bucket/prefix/")
    out_dir = await downloader.download(
        "tos://bucket/prefix/",
        str(tmp_path),
        credentials={
            "TOS_ACCESS_KEY": "AK",
            "TOS_SECRET_KEY": "SK",
            "endpoint": "https://tos.example.com",
            "region": "cn-beijing",
        },
    )

    assert out_dir == str(tmp_path)
    assert (tmp_path / "a.bin").exists()
    assert (tmp_path / "sub" / "b.bin").exists()

    assert fake_tos.last_client is not None
    assert fake_tos.last_client.kwargs["ak"] == "AK"
    assert fake_tos.last_client.kwargs["sk"] == "SK"
    assert fake_tos.last_client.kwargs["endpoint"] == "https://tos.example.com"
    assert fake_tos.last_client.kwargs["region"] == "cn-beijing"


@pytest.mark.asyncio
async def test_tos_downloader_empty_prefix_raises(
    monkeypatch: pytest.MonkeyPatch, tmp_path
):
    """A TOS prefix with no real objects (only directory markers / the prefix
    itself) must raise so the service layer doesn't write the completion
    marker over an empty directory."""
    fake_tos: Any = types.ModuleType("tos")

    class FakeObj:
        def __init__(self, key: str):
            self.key = key

    class FakeListResp:
        def __init__(self, keys: list[str]):
            self.contents = [FakeObj(key) for key in keys]
            self.is_truncated = False
            self.next_continuation_token = None

    class EmptyTosClient:
        def __init__(self, **kwargs):
            pass

        def list_objects_type2(self, bucket_name: str, **kwargs):
            prefix = kwargs.get("prefix", "")
            # Only directory markers — no real objects under the prefix
            return FakeListResp([prefix, f"{prefix}sub/"])

        def download_file(self, *a, **kw):  # pragma: no cover - unused
            raise AssertionError("should not be called")

    fake_tos.TosClientV2 = EmptyTosClient

    fake_tos_exceptions: Any = types.ModuleType("tos.exceptions")

    class TosClientError(Exception):
        pass

    class TosServerError(Exception):
        pass

    fake_tos_exceptions.TosClientError = TosClientError
    fake_tos_exceptions.TosServerError = TosServerError
    monkeypatch.setitem(sys.modules, "tos", fake_tos)
    monkeypatch.setitem(sys.modules, "tos.exceptions", fake_tos_exceptions)

    downloader = get_downloader("tos://bucket/missing-prefix/")
    with pytest.raises(FileNotFoundError, match="No objects found"):
        await downloader.download(
            "tos://bucket/missing-prefix/",
            str(tmp_path),
            credentials={
                "TOS_ACCESS_KEY": "AK",
                "TOS_SECRET_KEY": "SK",
                "endpoint": "https://tos.example.com",
                "region": "cn-beijing",
            },
        )


@pytest.mark.asyncio
async def test_tos_downloader_download_file(monkeypatch: pytest.MonkeyPatch, tmp_path):
    _install_fake_tos(monkeypatch)

    downloader = get_downloader("tos://bucket/path/file.bin")
    out_dir = await downloader.download(
        "tos://bucket/path/file.bin",
        str(tmp_path),
        credentials={
            "TOS_ACCESS_KEY": "AK",
            "TOS_SECRET_KEY": "SK",
            "endpoint": "https://tos.example.com",
            "region": "cn-beijing",
        },
    )

    assert out_dir == str(tmp_path)
    assert (tmp_path / "file.bin").exists()
