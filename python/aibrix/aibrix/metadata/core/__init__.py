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

from typing import TYPE_CHECKING

from .asyncio_thread import AsyncLoopThread, T
from .httpx_client import HTTPXClientWrapper

__all__ = ["AsyncLoopThread", "HTTPXClientWrapper", "KopfOperatorWrapper", "T"]

if TYPE_CHECKING:
    from .kopf_operator import KopfOperatorWrapper


def __getattr__(name: str):
    """Lazy-load Kopf (heavy) only when KopfOperatorWrapper is requested.

    Importing ``kopf`` pulls in optional kubernetes-asyncio typing; keeping this
    lazy avoids loading the operator stack for code paths that only need
    ``AsyncLoopThread`` (e.g. storage utilities and batch driver imports).
    """
    if name == "KopfOperatorWrapper":
        from .kopf_operator import KopfOperatorWrapper

        return KopfOperatorWrapper
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
