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

import pytest

from aibrix.common.errors import (
    ArgNotFormatError,
    ModelNotFoundError,
)
from aibrix.downloader.base import get_downloader
from aibrix.downloader.huggingface import HuggingFaceDownloader


def test_get_downloader_hf():
    downloader = get_downloader("facebook/opt-125m")
    assert isinstance(downloader, HuggingFaceDownloader)


def test_get_downloader_hf_not_exist():
    with pytest.raises(ModelNotFoundError) as exception:
        get_downloader("not_exsit_path/model")
    assert "Model not found" in str(exception.value)


def test_get_downloader_hf_invalid_uri():
    with pytest.raises(ArgNotFormatError) as exception:
        get_downloader("single_field")
    assert "not in the expected format: repo/name" in str(exception.value)

    with pytest.raises(ArgNotFormatError) as exception:
        get_downloader("multi/filed/repo")
    assert "not in the expected format: repo/name" in str(exception.value)
