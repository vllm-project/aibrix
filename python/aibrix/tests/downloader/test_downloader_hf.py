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

from aibrix.downloader.base import get_downloader
from aibrix.downloader.huggingface import HuggingFaceDownloader


def test_get_downloader_hf():
    downloader = get_downloader("facebook/opt-125m")
    assert isinstance(downloader, HuggingFaceDownloader)


def test_get_downloader_hf_not_exist():
    with pytest.raises(AssertionError) as exception:
        get_downloader("not_exsit_path/model")
    assert "not exist" in str(exception.value)


def test_get_downloader_hf_invalid_uri():
    with pytest.raises(AssertionError) as exception:
        get_downloader("single_field")
    assert "Model uri must be in `repo/name` format." in str(exception.value)

    with pytest.raises(AssertionError) as exception:
        get_downloader("multi/filed/repo")
    assert "Model uri must be in `repo/name` format." in str(exception.value)
