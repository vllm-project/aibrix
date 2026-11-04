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

import hashlib
import json
from pathlib import Path
from types import SimpleNamespace

import pytest
import requests
from botocore.exceptions import ClientError

from aibrix import envs
from aibrix.downloader import download_model
from aibrix.downloader.model_cache import (
    discover_prepared_artifacts,
    prepare_model_cache,
)


class ObjectTransport:
    """Only the remote SDK boundary is fake; downloader and files are real."""

    def __init__(self):
        self.objects = {
            "models/model/config.json": b'{"model_type":"llama"}',
            "models/model/tokenizer_config.json": b'{"tokenizer_class":"LlamaTokenizer"}',
            "models/model/tokenizer.json": b'{"version":"1.0"}',
            "models/model/model.safetensors": b"complete-weights",
        }
        self.payloads = []
        self.fail = False
        self._http = requests.Session()

    def head_bucket(self, *args, **kwargs):
        pass

    def head_object(self, **kwargs):
        name = kwargs.get("Key", kwargs.get("key"))
        if name not in self.objects:
            raise ClientError({"Error": {"Code": "404"}}, "HeadObject")
        data = self.objects[name]
        etag = hashlib.sha256(data).hexdigest()
        if "Key" in kwargs:
            return {"ContentLength": len(data), "ETag": etag}
        return SimpleNamespace(content_length=len(data), etag=etag)

    def get_paginator(self, name):
        return self

    def paginate(self, **kwargs):
        yield {
            "Contents": [
                {"Key": k} for k in self.objects if k.startswith(kwargs["Prefix"])
            ]
        }

    def list_objects_type2(self, *args, **kwargs):
        return SimpleNamespace(
            contents=[
                SimpleNamespace(key=k)
                for k in self.objects
                if k.startswith(kwargs["prefix"])
            ],
            is_truncated=False,
        )

    def download_file(self, **kwargs):
        if self.fail:
            raise OSError("interrupted transfer")
        name = kwargs.get("Key", kwargs.get("key"))
        destination = kwargs.get("Filename", kwargs.get("file_path"))
        self.payloads.append(name)
        Path(destination).write_bytes(self.objects[name])

    def bucket(self, name):
        return self

    def get_blob(self, name):
        if name not in self.objects:
            return None
        data = self.objects[name]

        def download(destination):
            self.download_file(Key=name, Filename=destination)

        return SimpleNamespace(
            name=name,
            size=len(data),
            etag=hashlib.sha256(data).hexdigest(),
            download_to_filename=download,
        )

    def list_blobs(self, bucket, prefix, max_results=None):
        names = [k for k in self.objects if k.startswith(prefix)]
        return iter(self.get_blob(k) for k in names[:max_results])


@pytest.fixture(params=["s3", "tos-v1", "tos-v2", "gcs", "gs"])
def provider(request, monkeypatch, tmp_path):
    from aibrix.downloader import gcs, s3, tos

    transport = ObjectTransport()
    monkeypatch.setattr(s3.boto3, "client", lambda **kwargs: transport)
    monkeypatch.setattr(tos.tos, "TosClientV2", lambda **kwargs: transport)
    monkeypatch.setattr(gcs.storage, "Client", lambda **kwargs: transport)
    monkeypatch.setattr(
        envs, "DOWNLOADER_TOS_VERSION", "v1" if request.param == "tos-v1" else "v2"
    )
    monkeypatch.setattr(envs, "DOWNLOADER_LOCAL_DIR", str(tmp_path))
    monkeypatch.setattr(envs, "DOWNLOADER_NUM_THREADS", 2)
    scheme = request.param.split("-")[0]
    return f"{scheme}://bucket/models/model", transport, tmp_path


def test_prepare_then_unchanged_native_downloader_reuses_payloads(provider):
    uri, transport, root = provider
    prepared = prepare_model_cache(uri)
    assert prepared == root / "model"
    assert len(transport.payloads) == 4
    assert uri in discover_prepared_artifacts(root)
    loaded = download_model(uri)
    assert loaded == prepared
    assert len(transport.payloads) == 4, (
        "warm activation must not transfer weights again"
    )
    assert (loaded / "model.safetensors").read_bytes() == b"complete-weights"


def test_changed_remote_object_is_not_hidden_by_receipt(provider):
    uri, transport, root = provider
    prepare_model_cache(uri)
    transport.objects["models/model/model.safetensors"] = b"updated-weights!"
    loaded = download_model(uri)
    assert (loaded / "model.safetensors").read_bytes() == b"updated-weights!"
    assert len(transport.payloads) == 5
    assert uri not in discover_prepared_artifacts(root)


def test_lost_file_withdraws_hint_and_native_load_recovers(provider):
    uri, transport, root = provider
    path = prepare_model_cache(uri)
    (path / "model.safetensors").unlink()
    assert uri not in discover_prepared_artifacts(root)
    assert (download_model(uri) / "model.safetensors").is_file()


def test_failed_prepare_is_not_published(provider):
    uri, transport, root = provider
    transport.fail = True
    with pytest.raises(Exception, match="interrupted"):
        prepare_model_cache(uri)
    assert discover_prepared_artifacts(root) == []


def test_receipt_cannot_point_outside_cache(provider, tmp_path):
    uri, transport, root = provider
    prepare_model_cache(uri)
    receipt = next((root / ".aibrix/prepared").glob("*.json"))
    data = json.loads(receipt.read_text())
    data["model_dir"] = "../../private"
    receipt.write_text(json.dumps(data))
    assert discover_prepared_artifacts(root) == []


@pytest.mark.parametrize(
    "uri",
    ["s3://bucket/a/../model", "s3://user:secret@bucket/model", "http://server/model"],
)
def test_prepare_rejects_unsupported_or_unsafe_source_before_io(uri):
    with pytest.raises(ValueError):
        prepare_model_cache(uri)


def test_relative_cache_root_is_supported(provider, monkeypatch):
    uri, transport, root = provider
    monkeypatch.chdir(root)
    prepared = prepare_model_cache(uri, "relative")
    assert prepared.resolve() == root / "relative/model"
    assert uri in discover_prepared_artifacts(root / "relative")


def test_bad_neighbor_receipt_does_not_hide_good_preparation(provider):
    uri, transport, root = provider
    prepare_model_cache(uri)
    (root / ".aibrix/prepared/bad.json").write_text('{"uri": []}')
    assert uri in discover_prepared_artifacts(root)


def test_preparation_does_not_follow_escaped_receipt_directory(provider, tmp_path):
    uri, transport, root = provider
    outside = tmp_path / "outside"
    outside.mkdir()
    (root / ".aibrix").symlink_to(outside)
    with pytest.raises(ValueError):
        prepare_model_cache(uri)
    assert not list(outside.iterdir())
    assert transport.payloads == []


def test_different_source_with_same_basename_is_rejected(provider):
    uri, transport, root = provider
    prepare_model_cache(uri)
    other = uri.replace("models/model", "different/model")
    with pytest.raises(FileExistsError, match="separate cache roots"):
        prepare_model_cache(other)
    assert uri in discover_prepared_artifacts(root)


def test_gcs_aliases_share_the_same_preparation(provider):
    uri, transport, root = provider
    if not uri.startswith(("gs:", "gcs:")):
        return
    prepare_model_cache("gs://bucket/models/model")
    prepare_model_cache("gcs://bucket/models/model")
    assert len(transport.payloads) == 4


def test_endpoint_change_withdraws_object_hint(provider, monkeypatch):
    uri, transport, root = provider
    prepare_model_cache(uri)
    if uri.startswith("s3:"):
        monkeypatch.setattr(
            envs, "DOWNLOADER_AWS_ENDPOINT_URL", "https://different-store"
        )
    elif uri.startswith("tos:"):
        monkeypatch.setattr(envs, "DOWNLOADER_TOS_ENDPOINT", "https://different-store")
    else:
        monkeypatch.setenv("STORAGE_EMULATOR_HOST", "https://different-store")
    assert discover_prepared_artifacts(root) == []


def test_empty_auxiliary_file_is_a_valid_preparation(provider):
    uri, transport, root = provider
    transport.objects["models/model/__init__.py"] = b""
    prepared = prepare_model_cache(uri)
    assert (prepared / "__init__.py").read_bytes() == b""
    assert uri in discover_prepared_artifacts(root)
    download_model(uri)
    assert transport.payloads.count("models/model/model.safetensors") == 1


def test_oversized_receipt_fails_before_publication(provider, monkeypatch):
    from aibrix.downloader import model_cache

    uri, transport, root = provider
    monkeypatch.setattr(model_cache, "MAX_METADATA_BYTES", 512, raising=False)
    with pytest.raises(ValueError, match="receipt.*limit"):
        prepare_model_cache(uri)
    assert not list((root / ".aibrix/prepared").glob("*.json"))
