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

from typing import Optional

from aibrix.downloader.base import get_downloader


def download_model(
    model_uri: str,
    local_path: Optional[str] = None,
    model_name: Optional[str] = None,
    enable_progress_bar: bool = False,
):
    """Download model from model_uri to local_path.

    Args:
        model_uri (str): model uri.
        local_path (str): local path to save model.
    """

    downloader = get_downloader(model_uri, model_name, enable_progress_bar)
    return downloader.download_model(local_path)


__all__ = ["download_model", "get_downloader"]
