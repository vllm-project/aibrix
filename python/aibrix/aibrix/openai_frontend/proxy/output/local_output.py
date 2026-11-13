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

"""Proxy-local batch persistence with pinned directories and storage-compatible sidecars."""

import asyncio
import hashlib
import json
import os
import stat
import uuid
from contextlib import ExitStack, contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import BinaryIO, Callable, Iterator, Optional, Union

from aibrix.storage.local import LocalStorage
from aibrix.storage.utils import _sanitize_key, generate_filename


def object_names(key: str, content_type: Optional[str] = None) -> tuple[str, str]:
    """Return the data filename and its ``.metadata`` sidecar for a storage key."""
    return generate_filename(key, content_type), f"{_sanitize_key(key)}.metadata"


class PinnedLocalOutput:
    """Pin proxy output batches with common storage backend APIs."""

    def __init__(self, storage: LocalStorage) -> None:
        self.base_path = storage.base_path

    @contextmanager
    def pinned_directory(self) -> Iterator[int]:
        """Pin the resolved root without following links during directory opens."""
        root = self.base_path.resolve(strict=True)
        expected = root.stat()
        with self._open_directory(root) as directory_fd:
            actual = os.fstat(directory_fd)
            if (actual.st_dev, actual.st_ino) != (expected.st_dev, expected.st_ino):
                raise OSError("Local output directory was replaced")
            yield directory_fd

    @staticmethod
    @contextmanager
    def _open_directory(path: Path, root_fd: Optional[int] = None) -> Iterator[int]:
        with ExitStack() as stack:
            if root_fd is None:
                directory_fd = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
                parts = path.parts[1:]
            else:
                directory_fd = os.dup(root_fd)
                parts = path.parts
            stack.callback(os.close, directory_fd)
            for part in parts:
                if root_fd is not None:
                    try:
                        os.mkdir(part, dir_fd=directory_fd)
                    except FileExistsError:
                        pass
                directory_fd = os.open(
                    part,
                    os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                    dir_fd=directory_fd,
                )
                stack.callback(os.close, directory_fd)
            yield directory_fd

    async def put_objects_at(
        self,
        entries: list[tuple[str, Union[bytes, BinaryIO], str]],
        check_destination: Callable[[int, str], None],
    ) -> bool:
        """Persist a batch against one pinned root and verify advertised locations.

        The caller must await the worker even on cancellation. All directories and
        readers remain owned until the entire batch has stopped writing.
        """
        await asyncio.get_running_loop().run_in_executor(
            None, self._put_objects_at, entries, check_destination
        )
        return True

    def _put_objects_at(
        self,
        entries: list[tuple[str, Union[bytes, BinaryIO], str]],
        check_destination: Callable[[int, str], None],
    ) -> None:
        with ExitStack() as stack:
            root_fd = stack.enter_context(self.pinned_directory())
            directories: dict[Path, int] = {Path("."): root_fd}
            for key, _, content_type in entries:
                for filename in object_names(key, content_type):
                    path = Path(filename)
                    if path.parent not in directories:
                        directories[path.parent] = stack.enter_context(
                            self._open_directory(path.parent, root_fd)
                        )
                    check_destination(directories[path.parent], path.name)
            written: dict[Path, os.stat_result] = {}
            for key, data, content_type in entries:
                payload = data if isinstance(data, bytes) else data.read()
                written.update(
                    self._put_object_at(
                        directories, key, payload, content_type, check_destination
                    )
                )
            # A fixed fd protects writes, but a changed path cannot be advertised
            # as a successful receipt. Check the whole batch, not just its last key.
            for path, fd in directories.items():
                expected = os.fstat(fd)
                try:
                    actual = (self.base_path / path).stat()
                except OSError as exc:
                    raise OSError(
                        "Local output directory changed during persistence"
                    ) from exc
                if (actual.st_dev, actual.st_ino) != (expected.st_dev, expected.st_ino):
                    raise OSError("Local output directory changed during persistence")
            for path, expected in written.items():
                actual = (self.base_path / path).lstat()
                if (
                    actual.st_dev,
                    actual.st_ino,
                    actual.st_size,
                    actual.st_mtime_ns,
                ) != (
                    expected.st_dev,
                    expected.st_ino,
                    expected.st_size,
                    expected.st_mtime_ns,
                ):
                    raise OSError("Local output changed during persistence")

    def _put_object_at(
        self,
        directories: dict[Path, int],
        key: str,
        data: bytes,
        content_type: str,
        check_destination: Callable[[int, str], None],
    ) -> dict[Path, os.stat_result]:
        target, sidecar = (Path(n) for n in object_names(key, content_type))
        data_fd = directories[target.parent]
        metadata_fd = directories[sidecar.parent]
        # Check both destinations before writing either member of the pair.
        check_destination(data_fd, target.name)
        check_destination(metadata_fd, sidecar.name)
        created_at = datetime.now(timezone.utc).isoformat()
        try:
            fd = os.open(
                sidecar.name,
                os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                dir_fd=metadata_fd,
            )
            with os.fdopen(fd, "r", encoding="utf-8") as reader:
                if stat.S_ISREG(os.fstat(reader.fileno()).st_mode):
                    previous = json.load(reader).get("created_at")
                    if isinstance(previous, str) and previous:
                        created_at = previous
        except (OSError, ValueError, AttributeError):
            pass
        info = self._replace_at(data_fd, target.name, data, check_destination)
        etag = hashlib.md5(f"{info.st_size}-{info.st_mtime}".encode()).hexdigest()
        metadata = {
            "created_at": created_at,
            "content_type": content_type,
            "metadata": {},
            "content_length": info.st_size,
            "etag": etag,
            "last_modified": datetime.fromtimestamp(
                info.st_mtime, timezone.utc
            ).isoformat(),
        }
        metadata_info = self._replace_at(
            metadata_fd,
            sidecar.name,
            json.dumps(metadata).encode("utf-8"),
            check_destination,
        )
        return {target: info, sidecar: metadata_info}

    @staticmethod
    def _replace_at(
        directory_fd: int,
        name: str,
        data: bytes,
        check_destination: Callable[[int, str], None],
    ) -> os.stat_result:
        temporary = f".aibrix-{uuid.uuid4().hex}.tmp"
        fd = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            0o600,
            dir_fd=directory_fd,
        )
        try:
            with os.fdopen(fd, "wb") as writer:
                writer.write(data)
                writer.flush()
                os.fsync(writer.fileno())
                info = os.fstat(writer.fileno())
            check_destination(directory_fd, name)
            os.replace(
                temporary, name, src_dir_fd=directory_fd, dst_dir_fd=directory_fd
            )
            return info
        finally:
            try:
                os.unlink(temporary, dir_fd=directory_fd)
            except FileNotFoundError:
                pass
