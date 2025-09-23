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

import asyncio
import os
import threading
from dataclasses import dataclass
from datetime import datetime
from typing import Optional

extension_map = {
    "image/jpeg": ".jpg",
    "application/x-tar": ".tar",
    "application/gzip": ".gz",
}


@dataclass
class ObjectMetadata:
    """Normalized object metadata across storage backends.

    This class provides a common interface for object metadata that
    normalizes the differences between S3, TOS, and local storage.
    """

    # Core metadata fields (available across all storage types)
    content_length: int
    content_type: Optional[str] = None
    etag: str = ""
    last_modified: Optional[datetime] = None
    metadata: Optional[dict[str, str]] = None

    # Extended fields (may not be available in all storage types)
    storage_class: Optional[str] = None
    version_id: Optional[str] = None
    encryption: Optional[str] = None
    checksum: Optional[str] = None

    # Additional metadata for special use cases
    cache_control: Optional[str] = None
    content_disposition: Optional[str] = None
    content_encoding: Optional[str] = None
    content_language: Optional[str] = None
    expires: Optional[datetime] = None


class AsyncLoopThread(threading.Thread):
    def __init__(self):
        super().__init__(daemon=True)
        self.loop = None
        self.started_event = threading.Event()

    def run(self):
        """The entry point for the new thread."""
        self.loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self.loop)

        # Signal that the loop is created and running
        self.started_event.set()

        # This will run until loop.stop() is called
        self.loop.run_forever()

        # Cleanly close the loop
        self.loop.close()

    def start(self):
        """Starts the thread and waits for the loop to be ready."""
        super().start()
        # Block until the 'run' method has set up the loop
        self.started_event.wait()

    def stop(self):
        """Stops the event loop and waits for the thread to exit."""
        if self.loop:
            # Schedule loop.stop() to be called from within the loop's thread
            self.loop.call_soon_threadsafe(self.loop.stop)
        self.join()

    def submit_coroutine(self, coro):
        """Submits a coroutine to the event loop from any thread."""
        return asyncio.run_coroutine_threadsafe(coro, self.loop)


storage_loop_thread: Optional[AsyncLoopThread] = None


def init_storage_loop_thread():
    global storage_loop_thread
    if storage_loop_thread is None:
        storage_loop_thread = AsyncLoopThread()
        storage_loop_thread.start()


def get_storage_loop_thread() -> Optional[AsyncLoopThread]:
    return storage_loop_thread


def stop_storage_loop_thread():
    global storage_loop_thread
    if storage_loop_thread:
        storage_loop_thread.stop()
        storage_loop_thread = None


def generate_filename(
    key: str,
    content_type: Optional[str] = None,
    metadata: Optional[dict[str, str]] = None,
) -> str:
    """Get full filesystem path for a key with path traversal protection."""
    # Sanitize the key to prevent path traversal attacks
    sanitized_key = _sanitize_key(key)

    # Check if the sanitized key already has an extension
    key_has_extension = "." in os.path.basename(sanitized_key)

    ext = ""

    # 1. Try to get extension from metadata's filename
    if metadata and (filename := metadata.get("filename")):
        # os.path.splitext correctly includes the dot (e.g., '.txt')
        ext = os.path.splitext(filename)[1]

    # 2. If no extension yet and key doesn't have extension, try to derive from content_type
    if ext == "" and not key_has_extension and content_type:
        if mapped_ext := extension_map.get(content_type):
            ext = mapped_ext
        elif "/" in content_type:
            # Safely get the subtype from a MIME type like 'image/jpeg'
            ext = content_type.split("/", 1)[1]
            # Sanitize the ext by keep digits alphabet
            ext = "." + "".join(c for c in ext if c.isalnum())

    # 3. Use pathlib for safer and more idiomatic path joining
    return sanitized_key + ext


def _sanitize_key(key: str) -> str:
    """Sanitize a key to prevent path traversal attacks."""
    # Remove any path traversal patterns
    # First, normalize the key by replacing backslashes and handling multiple slashes
    normalized = key.replace("\\", "/")

    # Remove multiple consecutive slashes
    while "//" in normalized:
        normalized = normalized.replace("//", "/")

    # Remove leading slash to prevent absolute paths
    normalized = normalized.lstrip("/")

    # Split by path separators and filter out dangerous components
    parts = []
    for part in normalized.split("/"):
        # Skip empty parts, current directory references, parent directory references,
        # and any part that contains only dots (like "...." which could be traversal attempts)
        if (
            part
            and part != "."
            and not part.startswith("..")
            and not all(c == "." for c in part)
        ):
            parts.append(part)

    # Join with forward slashes (will be handled properly by pathlib)
    sanitized = "/".join(parts)

    # If the original key was empty or only contained dangerous patterns, use a safe default
    if not sanitized:
        sanitized = "safe_key"

    return sanitized
