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
from io import StringIO
from typing import Any, Callable, Optional, Union

from .utils import get_storage_loop_thread


class SizeExceededError(Exception):
    pass


class Reader:
    """A unified synchronous wrapper for file-like objects.

    This wrapper provides a consistent file-like interface for reading from
    ANY file-like objects including BinaryIO, TextIO, FastAPI UploadFile,
    standard file objects, BytesIO, StringIO, and any custom file-like objects.

    The wrapper automatically detects the capabilities of the underlying object
    and adapts accordingly, making it truly universal for any file-like object.
    It supports both text and binary data, converting text to bytes when needed.

    Unlike the previous async version, this Reader provides a synchronous
    file-like interface that can be used directly with storage clients.
    """

    def __init__(
        self,
        file_obj: Any,
        cascade_close: bool = False,
        size_limiter: Optional[Union[Callable[[int, int], bool], int]] = None,
    ):
        """Initialize the Reader with any file-like object.

        Args:
            file_obj: Any object that implements file-like methods (read, seek, tell, close)
                     Can be BinaryIO, TextIO, FastAPI UploadFile, file objects, BytesIO, StringIO,
                     stream objects, or any custom object with file-like interface
            cascade_close: Whether to close the underlying file object when Reader is closed
            size_limiter: Optional callable that takes (bytes_read_so_far, bytes_to_read) and
                         returns True if read should proceed, False if it should be rejected.
                         If it returns False, a ValueError will be raised.
        """
        self._file_obj = file_obj
        self._closed = False

        # Detect object capabilities dynamically
        self._has_read = hasattr(file_obj, "read")
        self._has_seek = hasattr(file_obj, "seek")
        self._has_tell = hasattr(file_obj, "tell")
        self._has_close = hasattr(file_obj, "close")

        # Detect async methods (like FastAPI UploadFile)
        self._has_async_read = self._has_read and asyncio.iscoroutinefunction(
            file_obj.read
        )
        self._has_async_seek = self._has_seek and asyncio.iscoroutinefunction(
            file_obj.seek
        )
        self._has_async_tell = self._has_tell and asyncio.iscoroutinefunction(
            file_obj.tell
        )
        self._has_async_close = self._has_close and asyncio.iscoroutinefunction(
            file_obj.close
        )
        self._has_async = (
            self._has_async_read
            or self._has_async_seek
            or self._has_async_tell
            or self._has_async_close
        )

        # Special handling for FastAPI UploadFile-like objects
        self._is_upload_file_like = (
            hasattr(file_obj, "file")
            and hasattr(file_obj, "filename")
            and hasattr(file_obj, "read")
        )

        # Detect if this is a text file that needs conversion
        self._is_text_file = self._detect_text_file()

        # Buffer for TextIO objects to ensure correct byte-size reads
        self._text_buffer = b""

        # Track bytes read for non-seekable objects
        self._bytes_read = 0

        # Size limiter for controlling read operations
        self._size_limiter: Optional[Callable[[int, int], bool]] = None
        if isinstance(size_limiter, int):
            # Default implementation:
            # if bytes_to_read == -1: # read all
            #   bytes_read <= size_limiter
            # else:
            #   bytes_read + bytes_to_read <= size_limiter
            self._size_limiter = (
                lambda bytes_read, bytes_to_read: bytes_read
                + (bytes_to_read if bytes_read > 0 else 0)
                <= size_limiter
            )
        else:
            self._size_limiter = size_limiter

        # Cascade close
        self._cascade_close = cascade_close

        if self._has_async and not self._is_upload_file_like:
            assert get_storage_loop_thread() is not None

    def _detect_text_file(self) -> bool:
        """Detect if the underlying file object is a text file."""
        # Check for TextIO-like objects
        if hasattr(self._file_obj, "mode") and "b" not in str(self._file_obj.mode):
            return True
        # Check for StringIO
        if isinstance(self._file_obj, StringIO):
            return True
        # Check for objects that have encoding attribute (text files)
        if hasattr(self._file_obj, "encoding") and self._file_obj.encoding is not None:
            return True
        return False

    def _check_size_limit(self, bytes_to_read: int) -> None:
        """Check if the next read operation should be allowed based on size limits.

        Args:
            bytes_to_read: Number of bytes about to be read (-1 for all remaining)

        Raises:
            ValueError: If the size limiter rejects the read operation
        """
        if self._size_limiter is None:
            return

        # Call the size limiter with current bytes read and bytes to read
        try:
            allowed = self._size_limiter(self._bytes_read, bytes_to_read)
            if not allowed:
                raise SizeExceededError(
                    f"Read operation rejected by size limiter: {self._bytes_read} bytes already read, attempted to read {bytes_to_read} more bytes"
                )
        except Exception as e:
            if isinstance(e, SizeExceededError):
                raise
            # If the size limiter itself raises an exception, wrap it
            raise SizeExceededError(f"Size limiter check failed: {e}") from e

    def _read_text_with_byte_limit(self, size: int) -> bytes:
        """Read from TextIO object with correct byte size limiting.

        Note: This method does NOT update _bytes_read counter as it's called
        from the main read() method which handles the tracking.

        Args:
            size: Maximum number of bytes to read (-1 for all)

        Returns:
            Bytes read from the file, respecting byte limit
        """
        if size == -1:
            # Read all remaining data
            if not self._has_async_read:
                data = self._file_obj.read()
            elif self._is_upload_file_like:
                data = self._file_obj.file.read()
            else:
                storage_loop_thread = get_storage_loop_thread()
                assert storage_loop_thread is not None
                data = storage_loop_thread.submit_coroutine(
                    self._file_obj.read()
                ).result()

            # Return buffer + all remaining data
            if isinstance(data, str):
                remaining_bytes = data.encode("utf-8")
            else:
                remaining_bytes = bytes(data) if data else b""

            result = self._text_buffer + remaining_bytes
            self._text_buffer = b""
            return result

        # Need to read exactly 'size' bytes
        if len(self._text_buffer) >= size:
            # We have enough in buffer
            result = self._text_buffer[:size]
            self._text_buffer = self._text_buffer[size:]
            return result

        # Need to read more data from file
        result = self._text_buffer
        bytes_needed = size - len(self._text_buffer)
        self._text_buffer = b""

        # Read characters until we have enough bytes
        while len(result) < size:
            # Read a chunk of characters (not bytes!)
            # We use a small chunk size to avoid reading too much
            chunk_chars = min(bytes_needed, 1024)

            if not self._has_async_read:
                char_data = self._file_obj.read(chunk_chars)
            elif self._is_upload_file_like:
                char_data = self._file_obj.file.read(chunk_chars)
            else:
                storage_loop_thread = get_storage_loop_thread()
                assert storage_loop_thread is not None
                char_data = storage_loop_thread.submit_coroutine(
                    self._file_obj.read(chunk_chars)
                ).result()

            if not char_data:
                # End of file
                break

            # Convert to bytes
            if isinstance(char_data, str):
                chunk_bytes = char_data.encode("utf-8")
            else:
                chunk_bytes = bytes(char_data) if char_data else b""

            # Add to result
            available = result + chunk_bytes
            if len(available) <= size:
                # All fits
                result = available
            else:
                # Too much - keep some in buffer
                result = available[:size]
                self._text_buffer = available[size:]
                break

        return result

    def read(self, size: int = -1) -> bytes:
        """Read up to size bytes from the file.

        Args:
            size: Maximum number of bytes to read (-1 for all)

        Returns:
            Bytes read from the file

        Raises:
            ValueError: If the file is closed or object doesn't support read,
                       or if the size limiter rejects the read operation
        """
        if self._closed:
            raise ValueError("I/O operation on closed file")

        if not self._has_read:
            raise ValueError("Object does not support read operation")

        # Check size limit before reading
        self._check_size_limit(size)

        # Special handling for TextIO objects to ensure correct byte sizes
        if self._is_text_file:
            data = self._read_text_with_byte_limit(size)
        else:
            # Read data from the underlying object (binary mode)
            if not self._has_async_read:
                # Regular sync read
                data = self._file_obj.read(size)
            elif self._is_upload_file_like:
                # Special handling for FastAPI UploadFile - use the underlying .file attribute
                # which is typically a synchronous file-like object
                data = self._file_obj.file.read(size)
            else:
                storage_loop_thread = get_storage_loop_thread()
                assert storage_loop_thread is not None
                data = storage_loop_thread.submit_coroutine(
                    self._file_obj.read(size)
                ).result()

            # Ensure we always return bytes for binary data
            data = data if isinstance(data, bytes) else bytes(data)

        # Track bytes read for size calculation
        self._bytes_read += len(data)

        return data

    def seek(self, offset: int, whence: int = 0) -> int:
        """Change the stream position to the given byte offset.

        Args:
            offset: Offset in bytes
            whence: How to interpret the offset (0=absolute, 1=relative, 2=from end)

        Returns:
            New absolute offset

        Raises:
            ValueError: If the file is closed or object doesn't support seek
        """
        if self._closed:
            raise ValueError("I/O operation on closed file")

        if not self._has_seek:
            raise ValueError("Object does not support seek operation")

        # Clear text buffer when seeking (position will be invalidated)
        self._text_buffer = b""

        # Reset bytes read counter to match new position
        if whence == 0:  # Absolute positioning
            self._bytes_read = offset
        elif whence == 1:  # Relative to current position
            self._bytes_read += offset
        # For whence == 2 (relative to end), we'll update after the seek operation

        # Handle async seek for FastAPI UploadFile
        if not self._has_async_seek:
            new_pos = self._seek(offset, whence)
        elif self._is_upload_file_like and hasattr(self._file_obj.file, "seek"):
            # If _file_obj has no seek method, we don't seek it.
            # This part is to support sync operation.
            return self._file_obj.file.seek(offset, whence)
        else:
            storage_loop_thread = get_storage_loop_thread()
            assert storage_loop_thread is not None
            new_pos = storage_loop_thread.submit_coroutine(
                self._async_seek(offset, whence)
            ).result()

        # Update bytes read to match actual position
        if whence == 2:  # Relative to end
            self._bytes_read = new_pos

        return new_pos

    def _seek(self, offset: int, whence: int = 0) -> int:
        # Some objects might not support whence parameter
        try:
            return self._file_obj.seek(offset, whence)
        except TypeError:
            # Fallback to offset only for objects that don't support whence
            if whence == 0:
                return self._file_obj.seek(offset)
            else:
                raise ValueError("Object does not support seek with whence parameter")

    async def _async_seek(self, offset: int, whence: int = 0) -> int:
        # Some objects might not support whence parameter
        try:
            return await self._file_obj.seek(offset, whence)
        except TypeError:
            # Fallback to offset only for objects that don't support whence
            if whence == 0:
                return await self._file_obj.seek(offset)
            else:
                raise ValueError("Object does not support seek with whence parameter")

    def tell(self) -> int:
        """Return the current stream position.

        Note that we plan not to support tell for FastAPI UploadFile, which may affect
        the streaming efficiency if inproperly used to get file size.

        Returns:
            Current stream position in bytes

        Raises:
            ValueError: If the file is closed or object doesn't support tell
        """
        if self._closed:
            raise ValueError("I/O operation on closed file")

        if not self._has_tell:
            raise ValueError("Object does not support tell operation")

        if not self._has_async_tell:
            return self._file_obj.tell()
        elif self._is_upload_file_like and hasattr(self._file_obj.file, "tell"):
            # If _file_obj has no tell method, we don't tell it.
            # This part is to support sync operation.
            return self._file_obj.file.tell()
        else:
            storage_loop_thread = get_storage_loop_thread()
            assert storage_loop_thread is not None
            return storage_loop_thread.submit_coroutine(self._file_obj.tell()).result()

    def close(self) -> None:
        """Close the reader.

        Note: if cascade_close is set, any further operations on the file
        will raise ValueError after calling this method,
        """
        closed, self._closed = self._closed, True
        if closed:
            return

        if not self._has_close:
            return

        # Release any resources own by reader here.

        if not self._cascade_close:
            return

        if not self._has_async_close:
            self._file_obj.close()
        elif self._is_upload_file_like and hasattr(self._file_obj.file, "close"):
            # If _file_obj has no close method, we don't close it.
            # This part is to support sync operation.
            self._file_obj.file.close()
        else:
            storage_loop_thread = get_storage_loop_thread()
            assert storage_loop_thread is not None
            storage_loop_thread.submit_coroutine(self._file_obj.close())

    def get_size(self) -> int:
        """Get the size of the file in bytes.

        Returns:
            Size of the file in bytes

        Raises:
            ValueError: If the file is closed or size cannot be determined
        """
        if self._closed:
            raise ValueError("I/O operation on closed file")

        # Try to get size from object's size attribute (common in UploadFile-like objects)
        if hasattr(self._file_obj, "size") and self._file_obj.size is not None:
            return self._file_obj.size

        # For text files, seek positions are in characters, not bytes
        # We need to read the content to get the actual byte size
        if self._is_text_file and self._has_seek:
            current_pos = self.tell()
            try:
                # Read all content to get byte size
                self.seek(0)
                all_content = self.read(-1)
                return len(all_content)
            finally:
                # Restore original position
                self.seek(current_pos)

        # For binary files: seek to end, get position, then restore
        if not self._has_seek or not self._has_tell:
            raise ValueError("Cannot determine size - object doesn't support seek/tell")

        current_pos = self.tell()
        try:
            end_pos = self.seek(0, 2)  # Seek to end
            return end_pos
        finally:
            self.seek(current_pos)  # Restore original position

    @property
    def filename(self) -> Optional[str]:
        """Get the filename if available."""
        if hasattr(self._file_obj, "filename"):
            return getattr(self._file_obj, "filename", None)
        # Try common alternatives
        if hasattr(self._file_obj, "name"):
            return getattr(self._file_obj, "name", None)
        return None

    @property
    def content_type(self) -> Optional[str]:
        """Get the content type if available."""
        if hasattr(self._file_obj, "content_type"):
            return getattr(self._file_obj, "content_type", None)
        # Try common alternatives
        if hasattr(self._file_obj, "mimetype"):
            return getattr(self._file_obj, "mimetype", None)
        return None

    @property
    def closed(self) -> bool:
        """Check if the file is closed."""
        return self._closed

    def __enter__(self):
        """Context manager entry."""
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """Context manager exit."""
        self.close()

    def read_all(self) -> bytes:
        """Read all remaining data from the file.

        Returns:
            All remaining bytes in the file

        Raises:
            ValueError: If the size limiter rejects the read operation
        """
        data = self.read(-1)
        # Ensure we always return bytes
        if isinstance(data, str):
            return data.encode("utf-8")
        return data

    def read_chunk(self, chunk_size: int = 8192) -> bytes:
        """Read a chunk of data with specified size.

        Args:
            chunk_size: Size of chunk to read in bytes

        Returns:
            Chunk of data (may be smaller than chunk_size at end of file)

        Raises:
            ValueError: If the size limiter rejects the read operation
        """
        return self.read(chunk_size)

    def iter_chunks(self, chunk_size: int = 8192):
        """Iterator that yields chunks of data.

        Args:
            chunk_size: Size of each chunk in bytes

        Yields:
            Chunks of bytes data
        """
        while True:
            chunk = self.read_chunk(chunk_size)
            if not chunk:
                break
            yield chunk

    def __iter__(self):
        """Iterator for line-by-line reading.

        This provides compatibility with file-like interface.

        Yields:
            Lines from the file as bytes
        """
        return self.iter_lines()

    def iter_lines(self):
        """Iterator that yields lines from the file.

        Yields:
            Lines from the file as bytes (including newline characters)
        """
        buffer = b""
        chunk_size = 8192

        while True:
            chunk = self.read_chunk(chunk_size)
            if not chunk:
                # Yield any remaining buffer content
                if buffer:
                    yield buffer
                break

            buffer += chunk

            # Extract complete lines from buffer
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                yield line + b"\n"

    def is_binary(self) -> bool:
        """Check if the reader is binary.

        Returns:
            True if binary, False otherwise
        """
        return not self._is_text_file

    def readable(self) -> bool:
        """Check if the reader supports reading.

        Returns:
            True if readable, False otherwise
        """
        return self._has_read

    def seekable(self) -> bool:
        """Check if the reader supports seeking.

        Returns:
            True if seekable, False otherwise
        """
        return self._has_seek

    def tellable(self) -> bool:
        """Check if the reader supports telling.

        Returns:
            True if tellable, False otherwise
        """
        return self._has_tell

    def bytes_read(self) -> int:
        """Get the number of bytes read so far.

        Returns:
            Number of bytes read from the file
        """
        return self._bytes_read

    # Bytes-like object protocol methods
    def __bytes__(self) -> bytes:
        """Convert Reader to bytes by reading all content.

        This makes Reader work with file.write(reader) and other
        operations that expect bytes-like objects.

        Returns:
            All content as bytes
        """
        # For bytes-like conversion, we should read all content from beginning
        # and not worry about restoring position since this is a conversion operation
        try:
            # Save current state
            was_closed = self.closed

            # If closed, we can't read - return empty
            if was_closed:
                return b""

            # Try to seek to beginning if possible
            if self.seekable():
                try:
                    self.seek(0)
                except (ValueError, OSError):
                    # If seek fails, just read from current position
                    pass

            # Read all content
            return self.read_all()

        except Exception:
            # If anything fails, return empty bytes
            return b""

    def __len__(self) -> int:
        """Return the length of the content in bytes.

        Returns:
            Length of content in bytes
        """
        try:
            return self.get_size()
        except (ValueError, OSError):
            # If we can't determine size, try reading all and getting length
            try:
                self.read_all()
                return self._bytes_read
            except Exception:
                return 0

    def __getitem__(self, key) -> bytes:
        """Support slice notation on Reader.

        Args:
            key: slice object or integer index

        Returns:
            Sliced bytes content
        """
        # Get all content as bytes
        data = bytes(self)

        # Return slice of data
        return data[key]

    def __contains__(self, item) -> bool:
        """Check if bytes sequence is contained in Reader.

        Args:
            item: bytes sequence to search for

        Returns:
            True if item is found in content
        """
        try:
            data = bytes(self)
            return item in data
        except Exception:
            return False

    # Additional methods for bytes-like protocol
    def __eq__(self, other) -> bool:
        """Check equality with other bytes-like objects."""
        try:
            if isinstance(other, bytes):
                return bytes(self) == other
            elif hasattr(other, "__bytes__"):
                return bytes(self) == bytes(other)
            else:
                return False
        except Exception:
            return False

    def __str__(self) -> str:
        """Convert Reader to string (for text files).

        Returns:
            Content as string (decoded from bytes)
        """
        try:
            data = bytes(self)
            return data.decode("utf-8")
        except Exception:
            return ""

    def __hash__(self) -> int:
        """Hash method for Reader (based on content)."""
        try:
            return hash(bytes(self))
        except Exception:
            return hash(id(self))

    def __add__(self, other) -> bytes:
        """Concatenate Reader with other bytes-like objects."""
        try:
            self_bytes = bytes(self)
            if isinstance(other, bytes):
                return self_bytes + other
            elif hasattr(other, "__bytes__"):
                return self_bytes + bytes(other)
            else:
                raise TypeError(f"can't concatenate Reader and {type(other)}")
        except Exception:
            raise TypeError(
                f"unsupported operand type(s) for +: 'Reader' and '{type(other)}'"
            )

    def __radd__(self, other) -> bytes:
        """Right concatenate other bytes-like objects with Reader."""
        try:
            self_bytes = bytes(self)
            if isinstance(other, bytes):
                return other + self_bytes
            elif hasattr(other, "__bytes__"):
                return bytes(other) + self_bytes
            else:
                raise TypeError(f"can't concatenate {type(other)} and Reader")
        except Exception:
            raise TypeError(
                f"unsupported operand type(s) for +: '{type(other)}' and 'Reader'"
            )

    def __mul__(self, other) -> bytes:
        """Multiply Reader content by integer."""
        try:
            if isinstance(other, int):
                return bytes(self) * other
            else:
                raise TypeError(f"can't multiply Reader by {type(other)}")
        except Exception:
            raise TypeError(
                f"unsupported operand type(s) for *: 'Reader' and '{type(other)}'"
            )

    def __rmul__(self, other) -> bytes:
        """Right multiply Reader content by integer."""
        return self.__mul__(other)

    def __buffer__(self, flags):
        """Support for buffer protocol (Python 3.12+)."""
        return memoryview(bytes(self)).__buffer__(flags)

    def __release_buffer__(self, buffer):
        """Release buffer (Python 3.12+)."""
        pass

    def readline(self, size: int = -1) -> bytes:
        """Read and return one line from the stream.

        Args:
            size: Maximum number of bytes to read (-1 for no limit)

        Returns:
            Line as bytes (including newline character if present)

        Raises:
            ValueError: If the size limiter rejects any read operation
        """
        line = b""
        while True:
            char = self.read(1)
            if not char:
                break
            line += char
            if char == b"\n":
                break
            if size > 0 and len(line) >= size:
                break
        return line

    def readlines(self, hint: int = -1) -> list[bytes]:
        """Read and return a list of lines from the stream.

        Args:
            hint: Hint for number of lines to read (-1 for all)

        Returns:
            List of lines as bytes
        """
        lines = []
        for line in self.iter_lines():
            lines.append(line)
            if hint > 0 and len(lines) >= hint:
                break
        return lines
