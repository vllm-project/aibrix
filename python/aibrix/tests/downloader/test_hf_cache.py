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

import json
from pathlib import Path

import pytest

from aibrix.downloader.hf_cache import discover_cached_hf_artifacts


def snapshot(cache: Path, *, revision="a" * 40, ref="main", sharded=False):
    repo = cache / "models--Org--Qwen"
    directory = repo / "snapshots" / revision
    directory.mkdir(parents=True, exist_ok=True)
    (repo / "blobs").mkdir(exist_ok=True)
    if ref:
        (repo / "refs").mkdir(exist_ok=True)
        (repo / "refs" / ref).write_text(revision)
    files = {
        "config.json": json.dumps({"model_type": "qwen2"}).encode(),
        "tokenizer_config.json": b'{"tokenizer_class":"Qwen2Tokenizer"}',
        "tokenizer.json": b'{"version":"1.0"}',
    }
    if sharded:
        files.update(
            {
                "model.safetensors.index.json": b'{"weight_map":{"a":"model-1.safetensors","b":"model-2.safetensors"}}',
                "model-1.safetensors": b"weights-one",
                "model-2.safetensors": b"weights-two",
            }
        )
    else:
        files["model.safetensors"] = b"complete-weights"
    for name, content in files.items():
        blob = repo / "blobs" / (revision + "-" + name)
        blob.write_bytes(content)
        (directory / name).symlink_to(Path("../../blobs") / blob.name)
    return directory


def test_default_snapshot_advertises_existing_url_spellings(tmp_path):
    snapshot(tmp_path)
    assert discover_cached_hf_artifacts(tmp_path) == [
        "Org/Qwen",
        "hf://Org/Qwen",
        "huggingface://Org/Qwen",
    ]


def test_complete_shards_are_required(tmp_path):
    directory = snapshot(tmp_path, sharded=True)
    assert "hf://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)
    (directory / "model-2.safetensors").unlink()
    assert discover_cached_hf_artifacts(tmp_path) == []


@pytest.mark.parametrize(
    "name",
    ["config.json", "tokenizer.json", "tokenizer_config.json", "model.safetensors"],
)
def test_missing_required_file_withdraws_hint(tmp_path, name):
    directory = snapshot(tmp_path)
    (directory / name).unlink()
    assert discover_cached_hf_artifacts(tmp_path) == []


def test_old_snapshot_does_not_advertise_an_incomplete_main(tmp_path):
    snapshot(tmp_path)
    latest = snapshot(tmp_path, revision="b" * 40)
    (latest / "model.safetensors").unlink()
    assert discover_cached_hf_artifacts(tmp_path) == []


def test_unreferenced_snapshot_is_not_a_default_cache_hit(tmp_path):
    snapshot(tmp_path, ref=None)
    assert discover_cached_hf_artifacts(tmp_path) == []


@pytest.mark.parametrize("value", [b"broken", b"[]", b'{"model_type":null}'])
def test_bad_or_unsupported_config_is_not_advertised(tmp_path, value):
    directory = snapshot(tmp_path)
    (directory / "config.json").write_bytes(value)
    assert discover_cached_hf_artifacts(tmp_path) == []


def test_escaped_weight_link_is_rejected(tmp_path):
    directory = snapshot(tmp_path / "cache")
    outside = tmp_path / "private.safetensors"
    outside.write_bytes(b"private-weights")
    (directory / "model.safetensors").unlink()
    (directory / "model.safetensors").symlink_to(outside)
    assert discover_cached_hf_artifacts(tmp_path / "cache") == []


def test_corrupt_neighbor_does_not_hide_valid_repo(tmp_path):
    snapshot(tmp_path)
    (tmp_path / "models--Broken--Repo").mkdir()
    assert "hf://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)


def test_missing_cache_is_an_empty_hint(tmp_path):
    assert discover_cached_hf_artifacts(tmp_path / "absent") == []


@pytest.mark.parametrize(
    "index", [b'{"weight_map":{}}', b'{"weight_map":{"a":"../../secret"}}', b"[]"]
)
def test_invalid_shard_index_is_not_advertised(tmp_path, index):
    directory = snapshot(tmp_path, sharded=True)
    (directory / "model.safetensors.index.json").write_bytes(index)
    assert discover_cached_hf_artifacts(tmp_path) == []
