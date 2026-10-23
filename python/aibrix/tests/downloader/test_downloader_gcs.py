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

import types
from typing import Dict, List, Optional

import pytest
import requests
from google.auth.exceptions import DefaultCredentialsError
from requests.adapters import DEFAULT_POOLSIZE, HTTPAdapter

import aibrix.downloader.gcs as gcs_module
from aibrix.common.errors import (
    ArgNotCongiuredError,
    InvalidArgumentError,
    ModelNotFoundError,
)
from aibrix.downloader import download_model
from aibrix.downloader.base import DownloadExtraConfig, get_downloader
from aibrix.downloader.gcs import GCSDownloader


class FakeBlob:
    def __init__(self, client: "FakeClient", name: str, data: bytes):
        self.client = client
        self.name = name
        self.data = data
        self.size = len(data)
        self.etag = f"etag-{name}"

    def download_to_filename(self, filename: str):
        self.client.downloaded.append(self.name)
        with open(filename, "wb") as f:
            f.write(self.data)


class FakeBucket:
    def __init__(self, client: "FakeClient", name: str):
        self.client = client
        self.name = name

    def get_blob(self, blob_name: str) -> Optional[FakeBlob]:
        data = self.client.objects.get(self.name, {}).get(blob_name)
        return None if data is None else FakeBlob(self.client, blob_name, data)


class FakeClient:
    """In-memory stand-in for google.cloud.storage.Client."""

    def __init__(self, objects: Dict[str, Dict[str, bytes]]):
        self.objects = objects
        self.downloaded: List[str] = []
        self.list_error: Optional[Exception] = None
        self._http = requests.Session()

    def bucket(self, bucket_name: str) -> FakeBucket:
        return FakeBucket(self, bucket_name)

    def list_blobs(self, bucket_name, prefix=None, max_results=None):
        if self.list_error is not None:
            raise self.list_error
        names = sorted(
            n for n in self.objects.get(bucket_name, {}) if n.startswith(prefix or "")
        )
        if max_results is not None:
            names = names[:max_results]
        return iter(FakeBlob(self, n, self.objects[bucket_name][n]) for n in names)


@pytest.fixture
def gcs_client(monkeypatch):
    client = FakeClient(
        {
            "bucket": {
                "models/llama/config.json": b"{}",
                "models/llama/model.safetensors": b"weights",
                "models/llama/tokenizer/vocab.txt": b"vocab",
                "models/llama/": b"",  # directory marker
                "models/llama-2/config.json": b"{}",  # sibling sharing a prefix
                "models/single.bin": b"single",
            }
        }
    )
    monkeypatch.setattr(
        gcs_module,
        "storage",
        types.SimpleNamespace(Client=lambda project=None: client),
    )
    return client


@pytest.mark.parametrize("scheme", ["gcs", "gs"])
def test_get_downloader_gcs(gcs_client, scheme):
    downloader = get_downloader(f"{scheme}://bucket/models/llama")
    assert isinstance(downloader, GCSDownloader)
    assert downloader.bucket_name == "bucket"
    assert downloader.bucket_path == "models/llama"
    assert downloader.model_name == "llama"


@pytest.mark.parametrize(
    "uri", ["azure://container/models/llama", "http://example.com/models/llama"]
)
def test_get_downloader_names_an_unsupported_scheme(uri):
    scheme = uri.split("://")[0]
    with pytest.raises(InvalidArgumentError, match=f"`{scheme}://`"):
        get_downloader(uri)


def test_get_downloader_gcs_empty_bucket(gcs_client):
    with pytest.raises(ArgNotCongiuredError, match="`bucket_name` is not configured"):
        get_downloader("gs://")


def test_get_downloader_gcs_empty_path(gcs_client):
    with pytest.raises(ArgNotCongiuredError, match="`bucket_path` is not configured"):
        get_downloader("gs://bucket/")


def test_get_downloader_gcs_without_credentials(monkeypatch):
    def no_credentials(project=None):
        raise DefaultCredentialsError("Your default credentials were not found.")

    monkeypatch.setattr(
        gcs_module, "storage", types.SimpleNamespace(Client=no_credentials)
    )
    with pytest.raises(ModelNotFoundError, match="GCS credentials not found"):
        get_downloader("gs://bucket/models/llama")


@pytest.mark.parametrize(
    "num_threads, pool_maxsize", [(32, 32), (DEFAULT_POOLSIZE - 1, DEFAULT_POOLSIZE)]
)
def test_gcs_connection_pool_fits_the_download_threads(
    gcs_client, num_threads, pool_maxsize
):
    get_downloader(
        "gs://bucket/models/llama",
        download_extra_config={"num_threads": num_threads},
    )
    adapter = gcs_client._http.get_adapter("https://")
    assert adapter._pool_maxsize == pool_maxsize


def test_gcs_connection_pool_keeps_a_mutual_tls_adapter(gcs_client):
    class MutualTlsAdapter(HTTPAdapter):
        pass

    mtls = MutualTlsAdapter()
    gcs_client._http.mount("https://", mtls)
    get_downloader("gs://bucket/models/llama")
    assert gcs_client._http.get_adapter("https://") is mtls


def test_get_downloader_gcs_inaccessible_bucket(gcs_client):
    gcs_client.list_error = Exception("403 Forbidden")
    with pytest.raises(ModelNotFoundError, match="403 Forbidden"):
        get_downloader("gs://bucket/models/llama")


@pytest.mark.parametrize(
    "path, expected",
    [
        ("models/llama/", True),  # trailing slash
        ("models/llama", True),  # prefix with children
        ("models/single.bin", False),  # exact object, no children
        ("models/missing", True),  # nothing there: listed, then reported empty
    ],
)
def test_gcs_is_directory(gcs_client, path, expected):
    downloader = get_downloader(f"gs://bucket/{path}")
    assert downloader._is_directory() is expected


def test_gcs_download_directory(gcs_client, tmp_path):
    model_path = download_model("gs://bucket/models/llama", local_path=str(tmp_path))

    assert model_path == tmp_path / "llama"
    assert (model_path / "config.json").read_bytes() == b"{}"
    assert (model_path / "model.safetensors").read_bytes() == b"weights"
    assert (model_path / "tokenizer" / "vocab.txt").read_bytes() == b"vocab"
    # The sibling prefix `models/llama-2/` is not part of the model.
    assert sorted(gcs_client.downloaded) == [
        "models/llama/config.json",
        "models/llama/model.safetensors",
        "models/llama/tokenizer/vocab.txt",
    ]
    assert not list(model_path.rglob("*.part"))


def test_gcs_download_skips_files_already_downloaded(gcs_client, tmp_path):
    download_model("gs://bucket/models/llama", local_path=str(tmp_path))
    gcs_client.downloaded.clear()

    download_model("gs://bucket/models/llama", local_path=str(tmp_path))
    assert gcs_client.downloaded == []


def test_gcs_download_honors_allow_file_suffix(gcs_client, tmp_path):
    download_model(
        "gs://bucket/models/llama",
        local_path=str(tmp_path),
        download_extra_config={"allow_file_suffix": ["safetensors"]},
    )
    # vocab.txt is kept by the default always-allow suffixes.
    assert sorted(gcs_client.downloaded) == [
        "models/llama/model.safetensors",
        "models/llama/tokenizer/vocab.txt",
    ]


def test_gcs_download_single_file(gcs_client, tmp_path):
    model_path = download_model(
        "gcs://bucket/models/single.bin",
        local_path=str(tmp_path),
        model_name="m",
    )
    assert (model_path / "single.bin").read_bytes() == b"single"


def test_gcs_download_empty_prefix(gcs_client, tmp_path):
    with pytest.raises(ModelNotFoundError, match="No objects found"):
        download_model("gs://bucket/models/missing", local_path=str(tmp_path))


def test_gcs_download_missing_object(gcs_client, tmp_path):
    downloader = GCSDownloader(
        "gs://bucket/models/llama", download_extra_config=DownloadExtraConfig()
    )
    with pytest.raises(ValueError, match="not exist"):
        downloader.download(tmp_path, "models/llama/gone.json", "bucket")
