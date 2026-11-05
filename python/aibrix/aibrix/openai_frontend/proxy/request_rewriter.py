# Copyright 2026 The Aibrix Team.
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

from __future__ import annotations

import copy
from typing import Any, Dict, List, Optional, Union

from aibrix.openai_frontend.common.artifact_fetcher import (
    ArtifactFetcher,
    FetchResult,
)
from aibrix.openai_frontend.proxy.request_inspector import RequestInspector


class RequestRewriter:
    def __init__(
        self,
        inspector: RequestInspector,
        fetcher: ArtifactFetcher,
        credentials_map: Optional[Dict[str, Dict]] = None,
    ):
        self._inspector = inspector
        self._fetcher = fetcher
        self._credentials_map = credentials_map or {}

    async def rewrite(
        self,
        request_dict: Dict[str, Any],
    ) -> tuple[Dict[str, Any], List[FetchResult]]:
        locations = self._inspector.extract_urls(request_dict)
        if not locations:
            return request_dict, []

        unique_urls = list({loc.url for loc in locations})
        url_creds: Dict[str, Dict] = {
            u: creds
            for u in unique_urls
            if (creds := self._credentials_map.get(u)) is not None
        }
        fetch_results = await self._fetcher.fetch_many(unique_urls, url_creds or None)

        url_to_local: Dict[str, str] = {}
        for result in fetch_results:
            url_to_local[result.original_url] = result.local_path

        rewritten = copy.deepcopy(request_dict)
        for loc in locations:
            local_path = url_to_local.get(loc.url)
            if local_path is not None:
                _set_at_path(rewritten, loc.path, local_path)

        return rewritten, fetch_results


def _set_at_path(
    obj: Any,
    path: List[Union[str, int]],
    value: Any,
) -> None:
    for key in path[:-1]:
        obj = obj[key]
    obj[path[-1]] = value
