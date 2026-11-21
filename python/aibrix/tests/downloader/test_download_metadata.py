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
from aibrix.downloader.metadata import discover_download_metadata


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
    monkeypatch.setenv("AIBRIX_MODEL_CACHE_METADATA_DIR", str(tmp_path / "metadata"))
    monkeypatch.setattr(envs, "DOWNLOADER_NUM_THREADS", 2)
    scheme = request.param.split("-")[0]
    return f"{scheme}://bucket/models/model", transport, tmp_path


def prepare(uri, root=None):
    return download_model(
        uri, local_path=root, download_extra_config={"prepare_cache": True}
    )


def test_ordinary_download_returns_path_and_publishes_generic_result(provider):
    uri, transport, root = provider
    result = download_model(uri)
    assert isinstance(result, Path)
    assert result == root / "model"
    assert uri in discover_download_metadata()
    document = json.loads(next((root / "metadata").glob("*.json")).read_text())
    assert document["relative_path"] == "model"
    assert "model/model.safetensors" in document["files"]
    assert str(root) not in json.dumps(document), (
        "metadata must not store producer mount paths"
    )
    download_model(uri)
    assert len(transport.payloads) == 4


def test_no_metadata_configuration_preserves_original_download_behavior(
    provider, monkeypatch
):
    uri, transport, root = provider
    monkeypatch.delenv("AIBRIX_MODEL_CACHE_METADATA_DIR")
    assert download_model(uri) == root / "model"
    assert not (root / "metadata").exists()
    assert discover_download_metadata() == []


def test_index_failure_is_best_effort_except_explicit_preparation(provider):
    uri, transport, root = provider
    (root / "metadata").write_text("not a directory")
    assert download_model(uri) == root / "model"
    with pytest.raises((OSError, ValueError)):
        prepare(uri)


def test_failed_download_never_publishes_completion(provider):
    uri, transport, root = provider
    transport.fail = True
    with pytest.raises(Exception, match="interrupted"):
        download_model(uri)
    assert not list((root / "metadata").glob("*.json"))


def test_missing_file_withdraws_hint_and_normal_load_registers_recovery(provider):
    uri, transport, root = provider
    result = prepare(uri)
    (result / "model.safetensors").unlink()
    assert discover_download_metadata() == []
    download_model(uri)
    assert uri in discover_download_metadata()
    assert transport.payloads.count("models/model/model.safetensors") == 2


def test_remote_update_still_uses_original_etag_checks(provider):
    uri, transport, root = provider
    prepare(uri)
    transport.objects["models/model/model.safetensors"] = b"updated-weights!"
    result = download_model(uri)
    assert (result / "model.safetensors").read_bytes() == b"updated-weights!"
    assert uri in discover_download_metadata()
    assert len(transport.payloads) == 5


def test_empty_auxiliary_and_unknown_model_layout_are_valid_download_results(provider):
    uri, transport, root = provider
    transport.objects = {
        "models/model/opaque.bin": b"opaque-format",
        "models/model/__init__.py": b"",
    }
    result = prepare(uri)
    assert (result / "__init__.py").read_bytes() == b""
    assert uri in discover_download_metadata(), (
        "registry must not guess model architecture/tokenizer requirements"
    )


def test_metadata_paths_survive_different_container_mount_names(provider, monkeypatch):
    uri, transport, root = provider
    prepare(uri)
    other_mount = root / "consumer-mount"
    other_mount.mkdir()
    (root / "model").rename(other_mount / "model")
    monkeypatch.setattr(envs, "DOWNLOADER_LOCAL_DIR", str(other_mount))
    assert uri in discover_download_metadata()


def test_same_basename_follows_existing_downloader_behavior_not_new_rejection(provider):
    uri, transport, root = provider
    prepare(uri)
    for key, value in list(transport.objects.items()):
        transport.objects[key.replace("models/model", "other/model")] = (
            value + b"-other"
        )
    other = uri.replace("models/model", "other/model")
    prepare(other)
    assert other in discover_download_metadata()
    assert uri not in discover_download_metadata()


def test_forged_or_malformed_neighbor_is_ignored(provider):
    uri, transport, root = provider
    prepare(uri)
    folder = root / "metadata"
    (folder / "bad.json").write_text('{"source": []}')
    valid = next(p for p in folder.glob("*.json") if p.name != "bad.json")
    record = json.loads(valid.read_text())
    record["relative_path"] = "../private"
    (folder / "escaped.json").write_text(json.dumps(record))
    assert uri in discover_download_metadata()
    valid.unlink()
    assert discover_download_metadata() == []


def test_source_context_change_withdraws_hint(provider, monkeypatch):
    uri, transport, root = provider
    prepare(uri)
    if uri.startswith("s3:"):
        monkeypatch.setattr(
            envs, "DOWNLOADER_AWS_ENDPOINT_URL", "https://another-store"
        )
    elif uri.startswith("tos:"):
        monkeypatch.setattr(envs, "DOWNLOADER_TOS_ENDPOINT", "https://another-store")
    else:
        monkeypatch.setenv("STORAGE_EMULATOR_HOST", "https://another-store")
    assert discover_download_metadata() == []


def test_ordinary_download_reaches_runtime_without_warmup_specific_registration(
    provider,
):
    from aibrix.runtime.model_cache import scan_prepared_artifacts

    uri, transport, root = provider
    download_model(uri)
    assert uri in scan_prepared_artifacts()


def test_filtered_download_is_recorded_but_not_claimed_complete(provider):
    uri, transport, root = provider
    download_model(uri, download_extra_config={"allow_file_suffix": ["json"]})
    record = json.loads(next((root / "metadata").glob("*.json")).read_text())
    assert record["complete"] is False
    assert discover_download_metadata() == []
    with pytest.raises(ValueError, match="unfiltered"):
        download_model(
            uri,
            download_extra_config={
                "allow_file_suffix": ["json"],
                "prepare_cache": True,
            },
        )


def test_previous_interrupted_object_does_not_poison_current_result(provider):
    uri, transport, root = provider
    prepare(uri)
    source = "gcs" if uri.startswith("gs:") else uri.split(":")[0]
    stale = root / "model/.cache" / source / "download/removed.bin.lock"
    stale.write_text("")
    assert prepare(uri) == root / "model"
    assert uri in discover_download_metadata()


def test_removed_objects_are_not_part_of_the_next_download_result(provider):
    uri, transport, root = provider
    transport.objects["models/model/obsolete.bin"] = b"old auxiliary data"
    prepare(uri)
    del transport.objects["models/model/obsolete.bin"]
    prepare(uri)
    record = json.loads(next((root / "metadata").glob("*.json")).read_text())
    assert not any("obsolete.bin" in name for name in record["files"])
    (root / "model/obsolete.bin").unlink()
    assert uri in discover_download_metadata()


def test_directory_markers_cannot_publish_old_payloads_as_new_result(provider):
    uri, transport, root = provider
    prepare(uri)
    for record in (root / "metadata").glob("*.json"):
        record.unlink()
    transport.objects = {"models/model/": b""}
    with pytest.raises(ValueError):
        prepare(uri)
    assert not list((root / "metadata").glob("*.json"))
