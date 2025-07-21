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

"""
Reader class tests.

Tests for the universal Reader wrapper that provides BinaryIO-like interface
for any file-like object including FastAPI UploadFile, BytesIO, StringIO, etc.
"""

import tempfile
from io import BytesIO, StringIO
from pathlib import Path

import pytest
from fastapi import UploadFile

from aibrix.storage.reader import Reader, SizeExceededError
from aibrix.storage.utils import init_storage_loop_thread, stop_storage_loop_thread


class TestReader:
    """Test Reader functionality with various file-like objects."""

    # @classmethod
    # def setup_class(cls):
    #     print("\n--- Setting up class: Connecting to DB ---")
    #     # Simulate an expensive connection setup
    #     cls.db = {"status": "connected", "data": [1, 2, 3]}

    # @classmethod
    # def teardown_class(cls):
    #     print("\n--- Tearing down class: Disconnecting from DB ---")
    #     # Simulate closing the connection
    #     cls.db = None

    @pytest.mark.asyncio
    async def test_reader_with_bytesio(self):
        """Test Reader with BytesIO objects."""
        test_data = b"Hello, BytesIO world!"
        bytes_io = BytesIO(test_data)

        reader = Reader(bytes_io)

        # Test basic properties
        assert reader.readable() is True
        assert reader.seekable() is True
        assert reader.closed is False

        # Test reading
        data = reader.read()
        assert data == test_data

        # Test seeking and telling
        reader.seek(0)
        position = reader.tell()
        assert position == 0

        # Test size before reading
        size = reader.get_size()
        assert size == len(test_data)
        assert reader.bytes_read() == 0

        # Test partial read
        partial = reader.read(5)
        assert partial == b"Hello"
        assert reader.bytes_read() == 5

        # Test get_size
        size = reader.get_size()
        assert size == len(test_data)

        reader.close()
        assert reader.closed is True

    @pytest.mark.asyncio
    async def test_reader_with_stringio(self):
        """Test Reader with StringIO objects (converts to bytes)."""
        test_data = "Hello, StringIO world! 🌍"
        expected_bytes = test_data.encode("utf-8")
        string_io = StringIO(test_data)

        reader = Reader(string_io)

        # Test basic properties
        assert reader.readable() is True
        assert reader.seekable() is True

        # Test size before reading - StringIO is seekable and can be reset
        size = reader.get_size()
        assert size == len(expected_bytes)
        assert reader.bytes_read() == 0

        # Test size after partial read (10 bytes)
        partial = reader.read(10)
        assert len(partial) == 10
        assert reader.bytes_read() == 10
        size_after_read = reader.get_size()
        assert size_after_read == len(expected_bytes)

        # Test seeking
        reader.seek(0)
        partial = reader.read(5)
        assert partial == b"Hello"

        # Test reading (should convert to bytes)
        reader.seek(0)
        data = reader.read()
        assert data == expected_bytes
        assert reader.get_size() == len(expected_bytes)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_binary_file(self):
        """Test Reader with BinaryIO (real file objects)."""
        test_data = b"Hello, BinaryIO world!"

        with tempfile.NamedTemporaryFile() as temp_file:
            # Write test data
            temp_file.write(test_data)
            temp_file.flush()
            temp_file.seek(0)

            reader = Reader(temp_file)

            # Test basic properties
            assert reader.readable() is True
            assert reader.seekable() is True

            # Test size before reading
            size = reader.get_size()
            assert size == len(test_data)
            assert reader.bytes_read() == 0

            # Test reading
            data = reader.read()
            assert data == test_data
            assert reader.get_size() == len(test_data)

            # Test seeking
            reader.seek(0)
            partial = reader.read(5)
            assert partial == b"Hello"
            assert reader.bytes_read() == 5
            assert reader.get_size() == len(test_data)

            # Test filename property
            assert reader.filename == temp_file.name

            reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_text_file(self):
        """Test Reader with TextIO (text file objects)."""
        test_data = "Hello, TextIO world! 🌍"
        expected_bytes = test_data.encode("utf-8")

        with tempfile.NamedTemporaryFile(mode="w+", encoding="utf-8") as temp_file:
            # Write test data
            temp_file.write(test_data)
            temp_file.flush()
            temp_file.seek(0)

            reader = Reader(temp_file)

            # Test size before reading - text files can be reset after size determination
            size = reader.get_size()
            assert size == len(expected_bytes)
            # After get_size(), position should be reset for seekable text files
            assert reader.bytes_read() == 0

            # Test reading (should convert to bytes)
            data = reader.read()
            assert data == expected_bytes
            assert reader.get_size() == len(expected_bytes)

            # Test seeking
            reader.seek(0)
            partial = reader.read(5)
            assert partial == b"Hello"
            assert reader.bytes_read() == 5
            assert reader.get_size() == len(expected_bytes)

            reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_fastapi_uploadfile(self):
        """Test Reader with FastAPI UploadFile objects."""
        test_data = b"Hello, UploadFile world!"
        test_filename = "test_upload.bin"
        test_content_type = "application/octet-stream"

        # Create a mock file-like object for UploadFile
        mock_file = BytesIO(test_data)

        # Create UploadFile instance
        upload_file = UploadFile(
            file=mock_file,
            filename=test_filename,
            headers={"content-type": test_content_type},
        )

        reader = Reader(upload_file)

        # Test basic properties
        assert reader.readable() is True
        assert reader.seekable() is True
        # UploadFile might not support seek consistently
        assert reader.tellable() is False

        # Test filename and content_type properties
        assert reader.filename == test_filename
        assert reader.content_type == test_content_type

        # Test size before reading - UploadFile size is None, so it reads all data
        with pytest.raises(ValueError):
            reader.get_size()
        # Insure nothing read
        assert reader.bytes_read() == 0

        # Test reading
        data = reader.read()
        assert data == test_data
        assert reader.bytes_read() == len(test_data)
        # Lenght always work, but this is a good time to test it without extra efficiency
        assert len(reader) == len(test_data)

        # Test seeking (if supported), no storage_loop_thread needed.
        try:
            reader.seek(0)
            partial = reader.read(5)
            assert partial == b"Hello"
            assert reader.bytes_read() == 5
        except (ValueError, TypeError):
            # Some UploadFile implementations don't support seek with whence
            pass

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_custom_async_file_object(self):
        """Test Reader with custom async file-like objects."""
        test_data = b"Hello, async custom world!"

        init_storage_loop_thread()

        class AsyncFileObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0
                self._closed = False

            async def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            async def seek(self, offset: int, whence: int = 0) -> int:
                if whence == 0:
                    self.position = offset
                elif whence == 1:
                    self.position += offset
                elif whence == 2:
                    self.position = len(self.data) + offset
                return self.position

            async def tell(self) -> int:
                return self.position

            async def close(self) -> None:
                self._closed = True

        custom_file = AsyncFileObject(test_data)
        reader = Reader(custom_file)

        assert reader._has_async_read is True
        assert reader._has_async_seek is True
        assert reader._has_async_close is True

        # Test reading
        data = reader.read()
        assert data == test_data

        # Test seeking
        reader.seek(0)
        partial = reader.read(5)
        assert partial == b"Hello"

        # Test tell
        position = reader.tell()
        assert position == 5

        reader.close()

        stop_storage_loop_thread

    @pytest.mark.asyncio
    async def test_reader_with_custom_sync_file_object(self):
        """Test Reader with custom sync file-like objects."""
        test_data = b"Hello, sync custom world!"

        class SyncFileObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0
                self._closed = False

            def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            def seek(self, offset: int, whence: int = 0) -> int:
                if whence == 0:
                    self.position = offset
                elif whence == 1:
                    self.position += offset
                elif whence == 2:
                    self.position = len(self.data) + offset
                return self.position

            def tell(self) -> int:
                return self.position

            def close(self) -> None:
                self._closed = True

        custom_file = SyncFileObject(test_data)
        reader = Reader(custom_file)

        # Test reading
        data = reader.read()
        assert data == test_data

        # Test seeking
        reader.seek(0)
        partial = reader.read(5)
        assert partial == b"Hello"

        # Test tell
        position = reader.tell()
        assert position == 5

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_iteration_capabilities(self):
        """Test Reader's iteration methods."""
        test_data = b"Line 1\nLine 2\nLine 3\nPartial line"
        bytes_io = BytesIO(test_data)
        reader = Reader(bytes_io)

        # Test chunk iteration
        reader.seek(0)
        chunks = []
        for chunk in reader.iter_chunks(chunk_size=8):
            chunks.append(chunk)

        # Verify chunks
        assert len(chunks) > 1
        assert b"".join(chunks) == test_data

        # Test line iteration
        reader.seek(0)
        lines = []
        for line in reader.iter_lines():
            lines.append(line)

        # Verify lines
        expected_lines = [b"Line 1\n", b"Line 2\n", b"Line 3\n", b"Partial line"]
        assert lines == expected_lines

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_binaryio_methods(self):
        """Test Reader's BinaryIO-like methods."""
        test_data = b"Line 1\nLine 2\nLine 3\n"
        bytes_io = BytesIO(test_data)
        reader = Reader(bytes_io)

        # Test readline
        line = reader.readline()
        assert line == b"Line 1\n"

        # Test readlines with hint
        reader.seek(0)
        lines = reader.readlines(hint=2)
        assert len(lines) == 2
        assert lines[0] == b"Line 1\n"
        assert lines[1] == b"Line 2\n"

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_context_manager(self):
        """Test Reader as async context manager."""
        test_data = b"Hello, context manager!"
        bytes_io = BytesIO(test_data)

        with Reader(bytes_io) as reader:
            data = reader.read()
            assert data == test_data
            assert reader.closed is False

        # Should be closed after exiting context
        assert reader.closed is True

    @pytest.mark.asyncio
    async def test_reader_with_objects_without_seek(self):
        """Test Reader with objects that don't support seek."""

        class NoSeekObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0

            def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

        test_data = b"Hello, no seek!"
        no_seek_obj = NoSeekObject(test_data)
        reader = Reader(no_seek_obj)

        # Should not be seekable
        assert reader.seekable() is False

        # Reading should still work
        data = reader.read()
        assert data == test_data

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_get_size_non_seekable(self):
        """Test Reader get_size with non-seekable objects."""

        class NonSeekableObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0

            def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            def close(self):
                pass

        test_data = b"Hello, non-seekable size test!"
        no_seek_obj = NonSeekableObject(test_data)
        reader = Reader(no_seek_obj)

        # Should not be seekable
        assert reader.seekable() is False
        assert reader.bytes_read() == 0

        # Test size before reading
        with pytest.raises(ValueError):
            reader.get_size()
        assert len(reader) == len(test_data)
        # After calling len(), all data should be read
        assert reader.bytes_read() == len(test_data)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_get_size_non_seekable_partial_read(self):
        """Test Reader get_size with non-seekable objects after partial reads."""

        class NonSeekableObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0

            def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            def close(self):
                pass

        test_data = b"Hello, partial non-seekable test!"
        no_seek_obj = NonSeekableObject(test_data)
        reader = Reader(no_seek_obj)

        # Should not be seekable
        assert reader.seekable() is False

        # Read some data first
        partial = reader.read(5)
        assert partial == b"Hello"
        assert reader.bytes_read() == 5

        # Now get size - should read remaining data
        with pytest.raises(ValueError):
            reader.get_size()
        assert len(reader) == len(test_data)
        # After calling get_size(), all data should be read
        assert reader.bytes_read() == len(test_data)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_get_size_with_size_attribute(self):
        """Test Reader get_size with objects that have a size attribute."""

        class ObjectWithSize:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0
                self.size = len(data)  # This should be used directly

            def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            def close(self):
                pass

        test_data = b"Hello, size attribute test!"
        obj_with_size = ObjectWithSize(test_data)
        reader = Reader(obj_with_size)

        # Should get size from .size attribute without reading
        size = reader.get_size()
        assert size == len(test_data)
        assert len(reader) == len(test_data)
        assert reader.bytes_read() == 0  # Should not have read any data

        # Verify we can still read normally
        partial = reader.read(5)
        assert partial == b"Hello"
        assert reader.bytes_read() == 5

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_bytes_read_tracking(self):
        """Test Reader bytes_read tracking with various operations."""
        test_data = b"Hello, bytes tracking test!"
        bytes_io = BytesIO(test_data)
        reader = Reader(bytes_io)

        # Initially no bytes read
        assert reader.bytes_read() == 0

        # Read some data
        chunk1 = reader.read(5)
        assert chunk1 == b"Hello"
        assert reader.bytes_read() == 5

        # Read more data
        chunk2 = reader.read(7)
        assert chunk2 == b", bytes"
        assert reader.bytes_read() == 12

        # Seek and check tracking
        reader.seek(0)
        assert reader.bytes_read() == 0  # Should reset on seek

        # Read all remaining
        all_data = reader.read(-1)
        assert all_data == test_data
        assert reader.bytes_read() == len(test_data)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_error_handling(self):
        """Test Reader error handling for various edge cases."""
        test_data = b"Hello, errors!"
        bytes_io = BytesIO(test_data)
        reader = Reader(bytes_io)

        # Test operations on closed reader
        reader.close()

        with pytest.raises(ValueError, match="I/O operation on closed file"):
            reader.read()

        with pytest.raises(ValueError, match="I/O operation on closed file"):
            reader.seek(0)

        with pytest.raises(ValueError, match="I/O operation on closed file"):
            reader.tell()

        with pytest.raises(ValueError, match="I/O operation on closed file"):
            reader.get_size()

        # bytes_read() should still work on closed files since it's just a counter
        bytes_read = reader.bytes_read()
        assert isinstance(bytes_read, int)

    @pytest.mark.asyncio
    async def test_reader_async_iteration_error(self):
        """Test that async iteration raises appropriate error in async context."""
        test_data = b"Hello, sync iteration!"
        bytes_io = BytesIO(test_data)
        reader = Reader(bytes_io)

        # Should raise error when trying to use sync iteration in async context
        with pytest.raises(TypeError, match="not an async iterable"):
            aiter(reader)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_empty_data(self):
        """Test Reader with empty data."""
        empty_bytes_io = BytesIO(b"")
        reader = Reader(empty_bytes_io)

        # Should handle empty data gracefully
        data = reader.read()
        assert data == b""

        size = reader.get_size()
        assert size == 0

        # Iteration should handle empty data
        chunks = []
        for chunk in reader.iter_chunks():
            chunks.append(chunk)
        assert chunks == []

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_large_data(self):
        """Test Reader with large data to verify streaming behavior."""
        # Create large test data (100KB)
        large_data = b"A" * 100000

        class TrackingBytesIO(BytesIO):
            def __init__(self, data):
                super().__init__(data)
                self.read_calls = []

            def read(self, size=-1):
                result = super().read(size)
                self.read_calls.append(len(result))
                return result

        tracking_io = TrackingBytesIO(large_data)
        reader = Reader(tracking_io)

        # Test streaming via iteration (this triggers chunked reading)
        chunks = []
        for chunk in reader.iter_chunks(chunk_size=8192):
            chunks.append(chunk)

        # Verify we got all the data
        reconstructed_data = b"".join(chunks)
        assert reconstructed_data == large_data

        # Verify multiple read calls (streaming behavior)
        assert (
            len(tracking_io.read_calls) > 1
        ), f"Expected multiple read calls, got {len(tracking_io.read_calls)}: {tracking_io.read_calls[:5]}"

        # Verify chunks are roughly the expected size
        assert len(chunks) > 10, "Expected many chunks for large data"

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_different_encodings(self):
        """Test Reader with different text encodings."""
        # Test with UTF-8 text containing emojis
        test_text = "Hello, 世界! 🌍🚀"
        string_io = StringIO(test_text)
        reader = Reader(string_io)

        data = reader.read()
        expected_bytes = test_text.encode("utf-8")
        assert data == expected_bytes

        reader.close()

        # Test with bytes containing non-UTF-8 data
        test_bytes = b"\x00\x01\x02\xff\xfe\xfd"
        bytes_io = BytesIO(test_bytes)
        reader = Reader(bytes_io)

        data = reader.read()
        assert data == test_bytes

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_basic(self):
        """Test Reader with basic size limiter functionality."""
        test_data = b"Hello, size limiter world!"
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 10 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            if bytes_to_read == -1:  # read_all
                return bytes_read + len(test_data[bytes_read:]) <= 10
            return bytes_read + bytes_to_read <= 10

        reader = Reader(bytes_io, size_limiter=size_limiter)

        # Should allow reading 5 bytes
        data1 = reader.read(5)
        assert data1 == b"Hello"
        assert reader.bytes_read() == 5

        # Should allow reading 5 more bytes (total 10)
        data2 = reader.read(5)
        assert data2 == b", siz"
        assert reader.bytes_read() == 10

        # Should reject reading more bytes
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read(1)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_read_all(self):
        """Test Reader size limiter with read_all operation."""
        test_data = b"Hello, world!"
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 5 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            if bytes_to_read == -1:  # read_all
                remaining = len(test_data) - bytes_read
                return bytes_read + remaining <= 5
            return bytes_read + bytes_to_read <= 5

        reader = Reader(bytes_io, size_limiter=size_limiter)

        # Should reject read_all since total data exceeds limit
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read_all()

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_none(self):
        """Test Reader with no size limiter (default behavior)."""
        test_data = b"Hello, no limiter!"
        bytes_io = BytesIO(test_data)

        reader = Reader(bytes_io)  # No size_limiter

        # Should allow reading all data without restrictions
        data = reader.read_all()
        assert data == test_data
        assert reader.bytes_read() == len(test_data)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_chunks(self):
        """Test Reader size limiter with chunk reading."""
        test_data = b"A" * 100  # 100 bytes
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 50 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 50

        reader = Reader(bytes_io, size_limiter=size_limiter)

        # Should allow reading chunks up to limit
        chunk1 = reader.read_chunk(30)
        assert len(chunk1) == 30
        assert reader.bytes_read() == 30

        chunk2 = reader.read_chunk(20)
        assert len(chunk2) == 20
        assert reader.bytes_read() == 50

        # Should reject reading more chunks
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read_chunk(10)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_iter_chunks(self):
        """Test Reader size limiter with chunk iteration."""
        test_data = b"A" * 100  # 100 bytes
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 45 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 45

        reader = Reader(bytes_io, size_limiter=size_limiter)

        chunks = []
        # Should stop when size limit is reached
        try:
            for chunk in reader.iter_chunks(chunk_size=20):
                chunks.append(chunk)
        except SizeExceededError as e:
            assert "Read operation rejected by size limiter" in str(e)

        # Should have read 40 bytes (2 chunks of 20) before hitting limit
        assert reader.bytes_read() == 40
        assert len(chunks) == 2
        assert len(chunks[0]) == 20
        assert len(chunks[1]) == 20

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_readline(self):
        """Test Reader size limiter with line reading."""
        test_data = b"Line 1\nLine 2\nLine 3\nLine 4\n"
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 15 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 15

        reader = Reader(bytes_io, size_limiter=size_limiter)

        # Should allow reading first two lines
        line1 = reader.readline()
        assert line1 == b"Line 1\n"
        assert reader.bytes_read() == 7

        line2 = reader.readline()
        assert line2 == b"Line 2\n"
        assert reader.bytes_read() == 14

        # Should reject reading more (would exceed 15 byte limit)
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.readline()

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_with_text_file(self):
        """Test Reader size limiter with text files."""
        test_data = "Hello, 世界! 🌍"  # Mix of ASCII and multi-byte characters
        string_io = StringIO(test_data)

        # Create a size limiter that allows up to 10 bytes
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 10

        reader = Reader(string_io, size_limiter=size_limiter)

        # Should allow reading 10 bytes
        data = reader.read(10)
        assert len(data) == 10
        assert reader.bytes_read() == 10

        # Should reject reading more
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read(1)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_with_async_file_object(self):
        """Test Reader with custom async file-like objects."""
        test_data = b"Hello, async custom world!"

        # Create a size limiter that allows up to 10 bytes
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 10

        init_storage_loop_thread()

        class AsyncFileObject:
            def __init__(self, data: bytes):
                self.data = data
                self.position = 0
                self._closed = False

            async def read(self, size: int = -1) -> bytes:
                if size == -1:
                    result = self.data[self.position :]
                    self.position = len(self.data)
                else:
                    result = self.data[self.position : self.position + size]
                    self.position += len(result)
                return result

            async def seek(self, offset: int, whence: int = 0) -> int:
                if whence == 0:
                    self.position = offset
                elif whence == 1:
                    self.position += offset
                elif whence == 2:
                    self.position = len(self.data) + offset
                return self.position

            async def tell(self) -> int:
                return self.position

            async def close(self) -> None:
                self._closed = True

        custom_file = AsyncFileObject(test_data)
        reader = Reader(custom_file, size_limiter=size_limiter)

        # Should allow reading 10 bytes
        data = reader.read(10)
        assert len(data) == 10
        assert reader.bytes_read() == 10

        # Should reject reading more
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read(1)

        reader.close()

        stop_storage_loop_thread

    @pytest.mark.asyncio
    async def test_reader_size_limiter_exception_handling(self):
        """Test Reader size limiter exception handling."""
        test_data = b"Hello, exception test!"
        bytes_io = BytesIO(test_data)

        # Create a size limiter that raises an exception
        def failing_size_limiter(bytes_read, bytes_to_read):
            raise RuntimeError("Limiter failed!")

        reader = Reader(bytes_io, size_limiter=failing_size_limiter)

        # Should wrap the exception
        with pytest.raises(
            SizeExceededError, match="Size limiter check failed: Limiter failed!"
        ):
            reader.read(5)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_dynamic(self):
        """Test Reader with dynamic size limiter that changes behavior."""
        test_data = b"Dynamic limiter test data!"
        bytes_io = BytesIO(test_data)

        # Create a dynamic size limiter that becomes more restrictive
        class DynamicLimiter:
            def __init__(self):
                self.call_count = 0

            def __call__(self, bytes_read, bytes_to_read):
                self.call_count += 1
                # First call: allow up to 15 bytes
                # Second call: allow up to 20 bytes
                # Third call and beyond: allow up to 10 bytes only
                if self.call_count <= 2:
                    return bytes_read + bytes_to_read <= 20
                else:
                    return bytes_read + bytes_to_read <= 10

        limiter = DynamicLimiter()
        reader = Reader(bytes_io, size_limiter=limiter)

        # First read: should succeed (within 20 byte limit)
        data1 = reader.read(10)
        assert len(data1) == 10

        # Second read: should succeed (total 15 bytes, within 20 byte limit)
        data2 = reader.read(5)
        assert len(data2) == 5
        assert reader.bytes_read() == 15

        # Third read: should fail (limiter now only allows 10 bytes total)
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read(1)

        reader.close()

    @pytest.mark.asyncio
    async def test_reader_size_limiter_seek_reset(self):
        """Test Reader size limiter behavior with seek operations."""
        test_data = b"Seek test data!"
        bytes_io = BytesIO(test_data)

        # Create a size limiter that allows up to 10 bytes total
        def size_limiter(bytes_read, bytes_to_read):
            return bytes_read + bytes_to_read <= 10

        reader = Reader(bytes_io, size_limiter=size_limiter)

        # Read some data
        data1 = reader.read(5)
        assert len(data1) == 5
        assert reader.bytes_read() == 5

        # Seek back to beginning (should reset bytes_read counter)
        reader.seek(0)
        assert reader.bytes_read() == 0

        # Should now allow reading again from the beginning
        data2 = reader.read(10)
        assert len(data2) == 10
        assert reader.bytes_read() == 10

        # Should reject reading more
        with pytest.raises(
            SizeExceededError, match="Read operation rejected by size limiter"
        ):
            reader.read(1)

        reader.close()


class TestReaderIntegration:
    """Test Reader integration with storage systems."""

    @pytest.mark.asyncio
    async def test_reader_with_local_storage_integration(self):
        """Test Reader integration with LocalStorage."""
        from aibrix.storage import LocalStorage

        test_data = b"Hello, LocalStorage integration!"

        with tempfile.TemporaryDirectory() as temp_dir:
            storage = LocalStorage(base_path=temp_dir)

            # Test storing via Reader
            bytes_io = BytesIO(test_data)
            reader = Reader(bytes_io)

            test_key = "test/reader_integration.bin"
            await storage.put_object(test_key, reader)

            # Verify the data was stored correctly
            retrieved_data = await storage.get_object(test_key)
            assert retrieved_data == test_data

            # Clean up
            await storage.delete_object(test_key)
            reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_multipart_upload(self):
        """Test Reader with multipart upload scenarios."""
        from aibrix.storage import LocalStorage

        # Create data larger than typical multipart threshold
        large_data = b"Large multipart data: " + b"X" * 10000

        with tempfile.TemporaryDirectory() as temp_dir:
            storage = LocalStorage(base_path=temp_dir)

            # Test multipart upload via Reader
            bytes_io = BytesIO(large_data)
            reader = Reader(bytes_io)

            test_key = "test/multipart_reader.bin"

            # Force multipart upload by using the multipart_upload method directly
            await storage.multipart_upload(
                test_key,
                reader,
                content_type="application/octet-stream",
                bysize=4096,  # Force chunking
            )

            # Verify the data was stored correctly
            retrieved_data = await storage.get_object(test_key)
            assert retrieved_data == large_data

            # Clean up
            await storage.delete_object(test_key)
            reader.close()

    @pytest.mark.asyncio
    async def test_reader_with_different_file_types_in_storage(self):
        """Test Reader with various file types being stored."""
        from aibrix.storage import LocalStorage

        with tempfile.TemporaryDirectory() as temp_dir:
            storage = LocalStorage(base_path=temp_dir)

            test_cases = [
                ("text", "Hello, text file! 🌍", "text/plain"),
                ("binary", b"\x00\x01\x02\xff\xfe\xfd", "application/octet-stream"),
                ("json", '{"key": "value", "number": 42}', "application/json"),
                ("csv", "name,age\nAlice,30\nBob,25", "text/csv"),
            ]

            for name, data, content_type in test_cases:
                # Create appropriate file-like object
                if isinstance(data, str):
                    file_obj = StringIO(data)
                    expected_bytes = data.encode("utf-8")
                else:
                    file_obj = BytesIO(data)
                    expected_bytes = data

                reader = Reader(file_obj)
                test_key = f"test/{name}_file"

                # Store via Reader
                await storage.put_object(test_key, reader, content_type=content_type)

                # Verify storage
                retrieved_data = await storage.get_object(test_key)
                assert retrieved_data == expected_bytes

                # Clean up
                await storage.delete_object(test_key)
                reader.close()

    @pytest.mark.asyncio
    async def test_reader_bytes_like_behavior_with_file_operations(self):
        """Test Reader bytes-like behavior with actual file operations."""
        import tempfile

        test_data = b"Hello, bytes-like world! This data will be written and read back."

        with tempfile.TemporaryDirectory() as temp_dir:
            # Test 1: Write Reader as bytes to a binary file
            bytes_io = BytesIO(test_data)
            reader = Reader(bytes_io)

            binary_file_path = Path(temp_dir) / "binary_test.bin"
            with open(binary_file_path, "wb") as f:
                f.write(bytes(reader))  # This is the key test - using Reader as bytes

            # Verify the file contains correct data
            with open(binary_file_path, "rb") as f:
                written_data = f.read()
            assert written_data == test_data

            reader.close()

            # Test 2: Write Reader as string to a text file
            text_data = "Hello, text world! 🌍"
            string_io = StringIO(text_data)
            reader = Reader(string_io)

            text_file_path = Path(temp_dir) / "text_test.txt"
            with open(text_file_path, "w", encoding="utf-8") as f:
                f.write(str(reader))  # This tests string conversion

            # Verify the file contains correct text
            with open(text_file_path, "r", encoding="utf-8") as f:
                written_text = f.read()
            assert written_text == text_data

            reader.close()

            # Test 3: Test bytes-like arithmetic operations
            data1 = b"Hello, "
            data2 = b"world!"

            reader1 = Reader(BytesIO(data1))
            reader2 = Reader(BytesIO(data2))

            # Test concatenation
            combined = bytes(reader1) + bytes(reader2)
            assert combined == b"Hello, world!"

            # Test multiplication
            reader3 = Reader(BytesIO(b"X"))
            multiplied = bytes(reader3) * 5
            assert multiplied == b"XXXXX"

            reader1.close()
            reader2.close()
            reader3.close()

    @pytest.mark.asyncio
    async def test_reader_performance_characteristics(self):
        """Test Reader performance characteristics with large data."""
        import time

        # Create 1MB of test data
        large_data = b"Performance test data: " + b"P" * (1024 * 1024 - 23)

        bytes_io = BytesIO(large_data)
        reader = Reader(bytes_io)

        # Test streaming read performance
        start_time = time.time()

        total_read = 0
        chunk_count = 0
        for chunk in reader.iter_chunks(chunk_size=8192):
            total_read += len(chunk)
            chunk_count += 1

        duration = time.time() - start_time

        # Verify we read all data
        assert total_read == len(large_data)

        # Verify reasonable number of chunks (1MB / 8KB ≈ 128 chunks)
        expected_chunks = len(large_data) // 8192 + (1 if len(large_data) % 8192 else 0)
        assert chunk_count == expected_chunks

        # Performance should be reasonable (less than 1 second for 1MB)
        assert duration < 1.0, f"Reading 1MB took {duration:.3f}s, which seems too slow"

        reader.close()
