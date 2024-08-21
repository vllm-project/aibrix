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

from pathlib import Path
from typing import List

from aibrix.downloader.base import BaseDownloader


class TOSDownloader(BaseDownloader):
    def __init__(self, model_uri):
        super().__init__(model_uri)

    def _check_config(self):
        pass

    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        return False

    def _directory_list(self, path: str) -> List[str]:
        return []

    def _support_range_download(self) -> bool:
        return True

    def download(self, path: str, local_path: Path, enable_range: bool = True):
        pass
