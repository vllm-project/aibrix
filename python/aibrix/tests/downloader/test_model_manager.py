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
from pathlib import Path
from typing import Any, List

import pytest
from huggingface_hub import HfApi

from aibrix.config import DOWNLOAD_CACHE_DIR
from aibrix.downloader.entity import RemoteSource
from aibrix.downloader.huggingface import HuggingFaceDownloader
from aibrix.openapi import model as model_module
from aibrix.openapi.model import ModelManager
from aibrix.openapi.protocol import DownloadModelRequest, ModelStatusCard

MODEL_URI = "org/model"
NESTED_FILES = ["config.json", "LLM/config.json", "a/b/weights.safetensors"]


class RecordingProcess:
    """Stands in for multiprocessing.Process; never starts anything."""

    instances: List["RecordingProcess"] = []

    def __init__(self, *args: Any, **kwargs: Any):
        self.args = args
        self.kwargs = kwargs
        self.started = False
        RecordingProcess.instances.append(self)

    def start(self) -> None:
        self.started = True


@pytest.fixture(autouse=True)
def fake_hf(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(HfApi, "repo_exists", lambda self, *a, **kw: True)
    monkeypatch.setattr(model_module, "Process", RecordingProcess)
    RecordingProcess.instances = []


def _prepare_hf_model(local_dir: Path, interrupted: bool) -> None:
    model_dir = local_dir.joinpath(MODEL_URI)
    cache_dir = model_dir.joinpath(
        (DOWNLOAD_CACHE_DIR % RemoteSource.HUGGINGFACE.value).strip("/")
    )
    for name in NESTED_FILES:
        data = model_dir.joinpath(name)
        meta = cache_dir.joinpath(f"{name}.metadata")
        lock = cache_dir.joinpath(f"{name}.lock")
        for path in (data, meta, lock):
            path.parent.mkdir(parents=True, exist_ok=True)
        meta.touch()
        lock.touch()
        # An interrupted nested download has metadata but no data file.
        if not (interrupted and name == "LLM/config.json"):
            data.touch()


def _download(local_dir: Path) -> ModelStatusCard:
    card = asyncio.run(
        ModelManager.model_download(
            DownloadModelRequest(model_uri=MODEL_URI, local_dir=str(local_dir))
        )
    )
    assert isinstance(card, ModelStatusCard)
    return card


def test_huggingface_downloader_reports_its_source():
    assert HuggingFaceDownloader._source is RemoteSource.HUGGINGFACE


def test_interrupted_nested_download_is_resumed(tmp_path: Path):
    _prepare_hf_model(tmp_path, interrupted=True)

    card = _download(tmp_path)

    assert len(RecordingProcess.instances) == 1
    assert RecordingProcess.instances[0].started
    assert card.model_status == "downloading"
    assert card.source == "huggingface"


def test_complete_nested_download_is_not_restarted(tmp_path: Path):
    _prepare_hf_model(tmp_path, interrupted=False)

    card = _download(tmp_path)

    assert RecordingProcess.instances == []
    assert card.model_status == "downloaded"
    assert card.source == "huggingface"
