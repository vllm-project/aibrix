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

"""Output destination for requests whose native schema has none.

The client names where results should land, the frontend persists them there
and reports what it wrote.  The destination here is a URL:

* ``tos://<bucket>/<prefix>`` / ``s3://<bucket>/<prefix>`` -- object storage,
  written through the storage built by ``create_storage_from_env``.
* ``/abs/dir`` or ``file:///abs/dir`` -- a directory on the local filesystem.
* ``http(s)://...`` -- ``PUT`` to the URL (e.g. a pre-signed URL).
"""

from __future__ import annotations

from functools import lru_cache
from typing import FrozenSet, List
from urllib.parse import unquote, urlparse

from pydantic import BaseModel, ConfigDict, Field, field_validator

_NON_OBJECT_STORAGE_TYPES = frozenset({"local", "redis", "auto"})
HTTP_SCHEMES = frozenset({"http", "https"})
LOCAL_SCHEMES = frozenset({"", "file"})


@lru_cache(maxsize=1)
def object_store_schemes() -> FrozenSet[str]:
    """URL schemes served by an object-storage backend.

    Derived from :class:`aibrix.storage.types.StorageType`: each object-store
    backend is addressable as ``<storage_type.value>://bucket/key``. Imported
    lazily so importing the schema does not pull in storage backends.
    """
    from aibrix.storage.types import StorageType

    return frozenset(
        st.value for st in StorageType if st.value not in _NON_OBJECT_STORAGE_TYPES
    )


def local_output_path(url: str) -> str:
    """Decode file URIs once; plain filesystem paths keep literal percent signs."""
    parsed = urlparse(url)
    return unquote(parsed.path, errors="strict") if parsed.scheme == "file" else url


class OutputRequest(BaseModel):
    """Where the frontend should persist the result of a request."""

    model_config = ConfigDict(extra="forbid")

    url: str = Field(
        description=(
            "Destination prefix: 'tos://bucket/prefix', 's3://bucket/prefix', or "
            "an absolute local directory ('/data/out' or 'file:///data/out'); "
            "an http(s) PUT URL is also accepted. The artifact name is derived "
            "from the response id (e.g. <chatcmpl-id>.json)."
        ),
        examples=["tos://my-bucket/results/run-1", "/data/out", "https://host/put"],
    )

    @field_validator("url")
    @classmethod
    def _validate_url(cls, v: str) -> str:
        v = v.strip()
        if not v:
            raise ValueError("'output.url' must not be empty")
        parsed = urlparse(v)
        scheme = parsed.scheme.lower()
        supported = object_store_schemes() | HTTP_SCHEMES | LOCAL_SCHEMES
        if scheme not in supported:
            raise ValueError(
                f"'output.url' scheme '{scheme}' is not supported; use one of "
                f"{', '.join(sorted(s + '://' for s in object_store_schemes()))}, "
                "file://, http(s)://, or an absolute path"
            )
        # Prefix URIs have no query/fragment semantics; joining an artifact would
        # put its name outside the path. Plain paths keep literal '?' and '#', and
        # HTTP PUT destinations retain their exact signed URL, including queries.
        if scheme and scheme not in HTTP_SCHEMES and ("?" in v or "#" in v):
            raise ValueError("non-HTTP 'output.url' must not contain query or fragment")
        if scheme in object_store_schemes() and not parsed.netloc:
            raise ValueError(f"'output.url' must include a bucket: {v}")
        if scheme in HTTP_SCHEMES and not parsed.hostname:
            raise ValueError(f"'output.url' must include a hostname: {v}")
        if scheme in LOCAL_SCHEMES:
            if scheme == "file":
                if parsed.netloc and parsed.netloc.lower() != "localhost":
                    raise ValueError(
                        f"local 'output.url' with file:// scheme must have an "
                        f"empty host or 'localhost', got '{parsed.netloc}'"
                    )
            path = local_output_path(v)
            if "\x00" in path:
                raise ValueError("local 'output.url' must not contain NUL")
            if not path.startswith("/"):
                raise ValueError("local 'output.url' must be an absolute path")
            if any(part == ".." for part in path.split("/")):
                raise ValueError("local 'output.url' must not contain '..'")
        return v


class OutputResponse(BaseModel):
    """What the frontend wrote for a request with an ``output`` spec.

    Returned as the ``output`` block; other response fields remain present.
    """

    url: str = Field(
        description=(
            "The destination prefix, exactly as requested. Join it with each "
            "entry in 'artifacts' to get the full location."
        )
    )
    artifacts: List[str] = Field(
        default_factory=list,
        description=(
            "Artifacts written, relative to 'url' (object key suffix or path "
            "inside the directory); the full URL for an http(s) PUT target"
        ),
    )
