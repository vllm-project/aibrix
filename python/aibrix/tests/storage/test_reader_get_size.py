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

from io import BytesIO, StringIO, TextIOWrapper

import pytest

from aibrix.storage.reader import Reader, SizeExceededError


def _binary_stream(text):
    return BytesIO(text.encode("utf-8"))


def _text_stream(text):
    return TextIOWrapper(BytesIO(text.encode("utf-8")), encoding="utf-8")


@pytest.mark.parametrize(
    "stream_factory",
    [_binary_stream, StringIO, _text_stream],
    ids=["binary", "stringio", "textio"],
)
@pytest.mark.parametrize("text", ["ABCD", "你AB", "é世界Z"])
@pytest.mark.parametrize("prefix_length", [1, 2, 3])
@pytest.mark.parametrize("query_size", [Reader.get_size, len], ids=["get_size", "len"])
def test_size_query_preserves_remaining_bytes(
    stream_factory, text, prefix_length, query_size
):
    expected = text.encode("utf-8")
    with stream_factory(text) as stream:
        reader = Reader(stream)
        assert reader.read(prefix_length) == expected[:prefix_length]
        bytes_read = reader.bytes_read()

        for _ in range(2):
            assert query_size(reader) == len(expected)
            assert reader.bytes_read() == bytes_read

        assert reader.read() == expected[prefix_length:]


@pytest.mark.parametrize(
    "stream_factory", [StringIO, _text_stream], ids=["stringio", "textio"]
)
def test_rejected_size_query_preserves_text_read_state(stream_factory):
    text = "你AB"
    expected = text.encode("utf-8")
    with stream_factory(text) as stream:
        reader = Reader(stream, size_limiter=4)
        prefix = reader.read(1)

        with pytest.raises(SizeExceededError):
            reader.get_size()

        assert reader.bytes_read() == 1
        assert prefix + reader.read(3) == expected[:4]
        with pytest.raises(SizeExceededError):
            reader.read()
