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

"""Credential isolation and multipart compatibility across transport chunks."""

import httpx
import pytest

from aibrix.openai_frontend.proxy.output.multipart_output import (
    MAX_METADATA_BYTES,
    MultipartOutputFilter,
)
from aibrix.openai_frontend.utils.utils import ClientError, ServerError

CONTENT_TYPE = 'multipart/form-data; boundary="aibrix-output-test"'
SECRET = b"https://uploads.example/video?signature=frontend-only-token"
OUTPUT = b'{"url":"' + SECRET + b'"}'


def multipart(parts):
    return httpx.Request(
        "POST",
        "http://frontend/v1/videos/sync",
        headers={"content-type": CONTENT_TYPE},
        files=parts,
    ).read()


@pytest.mark.parametrize("chunk_size", [1, 7, 97, 65536])
@pytest.mark.parametrize(
    "position", ["absent", "first", "middle", "last", "duplicates"]
)
def test_output_is_removed_without_changing_other_parts(chunk_size, position):
    # Include delimiter-like bytes in a file, duplicate native fields, and
    # ordered references. None may be lost when parser callbacks span chunks.
    parts = [
        ("model", (None, "first-model")),
        ("prompt", (None, "生成视频。")),
        ("input_references", ("first.png", b"\x00\xff\r\n--aibrix-output-testX")),
        ("model", (None, "last-model")),
        ("input_references", ("second.png", b"second\x00\xff", "image/png")),
        ("extra_params", (None, '{"steps":12}')),
    ]
    expected = multipart(parts)
    if position == "first":
        parts.insert(0, ("output", (None, OUTPUT)))
    elif position == "middle":
        parts.insert(3, ("output", (None, OUTPUT)))
    elif position == "last":
        parts.append(("output", (None, OUTPUT)))
    elif position == "duplicates":
        parts.insert(0, ("output", (None, OUTPUT)))
        parts.insert(3, ("output", ("secret.json", SECRET, "application/json")))
        parts.append(("output", (None, "null")))

    body = multipart(parts)
    parser = MultipartOutputFilter(CONTENT_TYPE)
    forwarded = b"".join(
        parser.feed(body[offset : offset + chunk_size])
        for offset in range(0, len(body), chunk_size)
    )
    parser.finish()
    assert forwarded == expected
    assert SECRET not in forwarded
    assert b'name="output"' not in forwarded
    assert parser.model == "last-model"
    output = parser.output_request()
    if position in ("absent", "duplicates"):
        assert output is None
    else:
        assert output.url == SECRET.decode()


@pytest.mark.parametrize(
    "value",
    [
        (None, b'{"url":"' + SECRET),
        (None, OUTPUT + b" " * MAX_METADATA_BYTES),
        ("secret.json", SECRET, "application/json"),
    ],
    ids=["invalid-json", "oversized", "file"],
)
def test_invalid_output_is_removed_before_deferred_validation(value):
    native = [("model", (None, "minimax-h3"))]
    body = multipart([("output", value), *native])
    parser = MultipartOutputFilter(CONTENT_TYPE)
    forwarded = b"".join(
        parser.feed(body[offset : offset + 4096])
        for offset in range(0, len(body), 4096)
    )
    parser.finish()
    assert forwarded == multipart(native)
    assert SECRET not in forwarded
    with pytest.raises(ClientError):
        parser.output_request()


@pytest.mark.parametrize("attribute", ["_error", "_output_error"])
def test_output_request_reraises_saved_error(attribute):
    error = ServerError("previous metadata failure")
    parser = MultipartOutputFilter(CONTENT_TYPE)
    setattr(parser, attribute, error)
    with pytest.raises(ServerError) as caught:
        parser.output_request()
    assert caught.value is error


def test_removing_only_part_preserves_closing_boundary():
    parser = MultipartOutputFilter(CONTENT_TYPE)
    forwarded = parser.feed(multipart([("output", (None, OUTPUT))]))
    parser.finish()
    assert forwarded == b"--aibrix-output-test--\r\n"
    assert parser.output_request().url == SECRET.decode()


@pytest.mark.parametrize("name", ["model", "output"])
@pytest.mark.parametrize("chunk_size", [13, 65536])
def test_oversized_headers_preserve_native_parts_and_remove_output(name, chunk_size):
    headers = {"X-Large": "x" * (MAX_METADATA_BYTES + 1)}
    native = [
        ("model", (None, "minimax-h3")),
        ("input_references", ("reference.png", b"image\x00\xff", "image/png")),
    ]
    if name == "model":
        native[0] = ("model", (None, "minimax-h3", "text/plain", headers))
        parts = native
    else:
        parts = [("output", (None, OUTPUT, "application/json", headers)), *native]
    body = multipart(parts)
    parser = MultipartOutputFilter(CONTENT_TYPE)
    forwarded = b"".join(
        parser.feed(body[offset : offset + chunk_size])
        for offset in range(0, len(body), chunk_size)
    )
    parser.finish()
    assert forwarded == multipart(native)
    assert SECRET not in forwarded
    assert parser.model == "minimax-h3"
    output = parser.output_request()
    if name == "output":
        assert output.url == SECRET.decode()
    else:
        assert output is None


def test_unparsed_headers_never_fall_back_to_raw_forwarding():
    parser = MultipartOutputFilter(CONTENT_TYPE)
    body = (
        b"--aibrix-output-test\r\n"
        b'Content-Disposition: form-data; name="output"\r\nBroken header\r\n\r\n'
        + SECRET
    )
    forwarded = bytearray()
    with pytest.raises(ClientError):
        for offset in range(0, len(body), 13):
            forwarded.extend(parser.feed(body[offset : offset + 13]))
    assert not forwarded


def test_truncated_native_upload_is_rejected():
    parser = MultipartOutputFilter(CONTENT_TYPE)
    parser.feed(b'--aibrix-output-test\r\nContent-Disposition: form-data; name="model"')
    with pytest.raises(ClientError, match="Incomplete"):
        parser.finish()
