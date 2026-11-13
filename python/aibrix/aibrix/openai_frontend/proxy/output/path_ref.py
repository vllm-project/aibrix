# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""File references with explicit ownership; identities alone cannot grant cleanup rights."""

from __future__ import annotations

import os
from collections.abc import Iterator, Mapping, MutableMapping
from dataclasses import dataclass, replace
from enum import Enum
from pathlib import Path
from typing import Optional


class PathOwnership(str, Enum):
    OWNED = "owned"
    EXTERNAL = "external"


@dataclass(frozen=True)
class PathRef:
    path: Path
    identity: Optional[os.stat_result]
    ownership: PathOwnership = PathOwnership.OWNED
    parent_identity: Optional[tuple[int, int]] = None
    # Owned input cleanup may belong to engine stop rather than output persistence.
    retain_until_stop: bool = False

    @classmethod
    def from_path(
        cls,
        path: Path,
        identity: Optional[os.stat_result],
        ownership: PathOwnership = PathOwnership.OWNED,
    ) -> PathRef:
        """Keep the declared directory entry even if the file is later removed."""
        parent = path.parent.stat()
        return cls(path, identity, ownership, (parent.st_dev, parent.st_ino))

    @classmethod
    def external(cls, path: Path, identity: os.stat_result) -> PathRef:
        return cls.from_path(path, identity, PathOwnership.EXTERNAL)

    def matches(self, path: Path, identity: Optional[os.stat_result]) -> bool:
        return self.path == path or (
            self.identity is not None
            and identity is not None
            and (self.identity.st_dev, self.identity.st_ino)
            == (identity.st_dev, identity.st_ino)
        )


class PathRefs(MutableMapping[Path, Optional[os.stat_result]]):
    """One reference ledger, with a mapping view for existing identity consumers.

    Updating a file's observed stat must not change its ownership. External
    references therefore stay external if an engine later declares them as output.
    """

    def __init__(self) -> None:
        self._refs: dict[Path, PathRef] = {}

    def add(self, ref: PathRef) -> None:
        current = self._refs.get(ref.path)
        if current is not None and current.ownership is PathOwnership.EXTERNAL:
            return
        if current is not None:
            ref = replace(
                ref,
                parent_identity=current.parent_identity or ref.parent_identity,
                retain_until_stop=current.retain_until_stop or ref.retain_until_stop,
            )
        self._refs[ref.path] = ref

    def remember(self, identities: Mapping[Path, Optional[os.stat_result]]) -> None:
        """Merge first observations, preserving complete references when available."""
        for path, identity in identities.items():
            if path not in self:
                self.add(
                    identities.ref(path)
                    if isinstance(identities, PathRefs)
                    else PathRef(path, identity)
                )

    def retain_until_stop(self, retain: bool) -> None:
        """Keep current owned inputs alive without changing their ownership."""
        for path, ref in self._refs.items():
            if ref.ownership is PathOwnership.OWNED:
                self._refs[path] = replace(ref, retain_until_stop=retain)

    def protected(self) -> tuple[PathRef, ...]:
        """References that response persistence must neither delete nor overwrite."""
        return tuple(
            ref
            for ref in self._refs.values()
            if ref.ownership is PathOwnership.EXTERNAL or ref.retain_until_stop
        )

    def ref(self, path: Path) -> PathRef:
        """Return the complete reference, including its ownership."""
        return self._refs[path]

    def __getitem__(self, path: Path) -> Optional[os.stat_result]:
        return self._refs[path].identity

    def __setitem__(self, path: Path, identity: Optional[os.stat_result]) -> None:
        current = self._refs.get(path)
        self._refs[path] = (
            replace(current, identity=identity)
            if current is not None
            else PathRef(path, identity)
        )

    def __delitem__(self, path: Path) -> None:
        del self._refs[path]

    def __iter__(self) -> Iterator[Path]:
        return iter(self._refs)

    def __len__(self) -> int:
        return len(self._refs)
