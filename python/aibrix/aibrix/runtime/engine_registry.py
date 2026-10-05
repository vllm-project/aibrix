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

"""Crash-safe local state for engines owned by one runtime sidecar."""

from __future__ import annotations

import json
import logging
import os
import tempfile
from pathlib import Path
from typing import Any, Dict, List, Union

logger = logging.getLogger(__name__)


class EngineRegistry:
    """Persist runtime-owned engine metadata across agent process restarts.

    The registry is deliberately local to the Pod. Kubernetes remains the
    desired-state authority while this file gives a replacement sidecar enough
    evidence to adopt the engine processes that survived its own restart.
    """

    _VERSION = 1

    def __init__(self, path: Union[str, Path]):
        self.path = Path(path)

    def load(self) -> List[Dict[str, Any]]:
        """Return the last complete engine set, or no records when unusable."""
        try:
            with self.path.open(encoding="utf-8") as registry_file:
                document = json.load(registry_file)
        except FileNotFoundError:
            return []
        except (OSError, UnicodeError, json.JSONDecodeError) as exc:
            logger.warning("could not load engine registry %s: %s", self.path, exc)
            return []

        if not isinstance(document, dict):
            logger.warning(
                "could not load engine registry %s: invalid document", self.path
            )
            return []
        if document.get("version") != self._VERSION:
            logger.warning(
                "could not load engine registry %s: unsupported version %r",
                self.path,
                document.get("version"),
            )
            return []
        engines = document.get("engines")
        if not isinstance(engines, list) or not all(
            isinstance(engine, dict) for engine in engines
        ):
            logger.warning(
                "could not load engine registry %s: invalid engines", self.path
            )
            return []
        return engines

    def save(self, engines: List[Dict[str, Any]]) -> None:
        """Atomically replace the persisted engine set.

        A same-directory temporary file followed by ``os.replace`` prevents a
        sidecar crash from leaving a partially-written registry in place.
        """
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temporary_path: Path | None = None
        try:
            fd, raw_temporary_path = tempfile.mkstemp(
                dir=self.path.parent,
                prefix=f".{self.path.name}.",
                suffix=".tmp",
            )
            temporary_path = Path(raw_temporary_path)
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as registry_file:
                json.dump(
                    {"version": self._VERSION, "engines": engines},
                    registry_file,
                    separators=(",", ":"),
                    sort_keys=True,
                )
                registry_file.flush()
                os.fsync(registry_file.fileno())
            os.replace(temporary_path, self.path)
            temporary_path = None
            self._sync_directory()
        finally:
            if temporary_path is not None:
                try:
                    temporary_path.unlink()
                except FileNotFoundError:
                    pass

    def _sync_directory(self) -> None:
        """Persist the rename metadata where the platform supports it."""
        try:
            directory_fd = os.open(
                self.path.parent,
                os.O_RDONLY | getattr(os, "O_DIRECTORY", 0),
            )
        except OSError as exc:
            logger.debug("could not open registry directory for fsync: %s", exc)
            return
        try:
            try:
                os.fsync(directory_fd)
            except OSError as exc:
                logger.debug("directory fsync is not supported: %s", exc)
        finally:
            os.close(directory_fd)
