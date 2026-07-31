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

import logging
from typing import Any, Callable
from urllib.parse import quote


class DelayedLog:
    def __init__(self, expr: Callable[[], Any]):
        self._expr = expr

    def __str__(self) -> str:
        return str(self._expr())

    def __expr__(self) -> str:
        return self.__str__()


class ExcludePathsFilter(logging.Filter):
    def __init__(self, exclude_paths):
        super().__init__()
        self.exclude_paths = [quote(exclude_path) for exclude_path in exclude_paths]

    def filter(self, record):
        # Check if the record is an access log and extract the path
        if hasattr(record, "args") and len(record.args) >= 3:
            request_path = record.args[2]
            return not any(request_path.startswith(path) for path in self.exclude_paths)
        return True  # Allow other log records
