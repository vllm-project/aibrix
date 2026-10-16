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

import pytest

from aibrix.runtime.artifact_service import ArtifactDelegationService

ADAPTER_FILES = ("adapter_config.json", "adapter_model.safetensors")


def _fake_snapshot_download(
    repo_id, cache_dir=None, local_dir=None, token=None, **kwargs
):
    """Mimic huggingface_hub.snapshot_download's two on-disk layouts.

    With ``local_dir`` the files are written flat into that directory. With
    ``cache_dir`` they are written under
    ``models--<org>--<name>/snapshots/<revision>/`` and that snapshot
    directory is returned.
    """
    if local_dir is not None:
        target = str(local_dir)
    else:
        target = os.path.join(
            str(cache_dir),
            "models--" + repo_id.replace("/", "--"),
            "snapshots",
            "0123abcd",
        )
    os.makedirs(target, exist_ok=True)
    for name in ADAPTER_FILES:
        with open(os.path.join(target, name), "w") as f:
            f.write("x")
    return target


@pytest.fixture
def fake_hf(monkeypatch):
    import huggingface_hub

    monkeypatch.setattr(huggingface_hub, "snapshot_download", _fake_snapshot_download)


@pytest.mark.asyncio
async def test_huggingface_adapter_is_downloaded_into_its_directory(tmp_path, fake_hf):
    service = ArtifactDelegationService(local_dir=str(tmp_path))

    path = await service.download_artifact("huggingface://org/adapter", "adapter")

    assert path == str(tmp_path / "adapter")
    for name in ADAPTER_FILES:
        assert os.path.isfile(os.path.join(path, name))


@pytest.mark.asyncio
async def test_huggingface_reload_returns_the_same_path(tmp_path, fake_hf):
    """A second load hits the completion marker and must hand the engine the
    same adapter directory as the first load."""
    service = ArtifactDelegationService(local_dir=str(tmp_path))

    first = await service.download_artifact("huggingface://org/adapter", "adapter")
    second = await service.download_artifact("huggingface://org/adapter", "adapter")

    assert second == first
    for name in ADAPTER_FILES:
        assert os.path.isfile(os.path.join(second, name))


@pytest.mark.asyncio
async def test_hf_alias_adapter_is_downloaded_into_its_directory(tmp_path, fake_hf):
    """The controller accepts ``hf://`` as an alias of ``huggingface://`` and
    sends the URL to the runtime unchanged."""
    service = ArtifactDelegationService(local_dir=str(tmp_path))

    path = await service.download_artifact("hf://org/adapter", "adapter")

    assert path == str(tmp_path / "adapter")
    for name in ADAPTER_FILES:
        assert os.path.isfile(os.path.join(path, name))
