# Copyright 2025-2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions
# are met:
#  * Redistributions of source code must retain the above copyright
#    notice, this list of conditions and the following disclaimer.
#  * Redistributions in binary form must reproduce the above copyright
#    notice, this list of conditions and the following disclaimer in the
#    documentation and/or other materials provided with the distribution.
#  * Neither the name of NVIDIA CORPORATION nor the names of its
#    contributors may be used to endorse or promote products derived
#    from this software without specific prior written permission.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS ``AS IS'' AND ANY
# EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR
# PURPOSE ARE DISCLAIMED.  IN NO EVENT SHALL THE COPYRIGHT OWNER OR
# CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
# EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
# PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
# PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY
# OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
# (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
# OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

import logging
from enum import IntEnum

# Default value for the --http-max-input-size CLI flag (64 MiB).
# Same as HTTP_DEFAULT_MAX_INPUT_SIZE in src/common.h.
HTTP_DEFAULT_MAX_INPUT_SIZE: int = 1 << 26


class ServerError(Exception):
    """Exception raised for server errors."""

    pass


class ClientError(Exception):
    """Exception raised for client errors."""

    pass


class StatusCode(IntEnum):
    SUCCESS = 200
    CLIENT_ERROR = 400
    AUTHORIZATION_ERROR = 401
    NOT_FOUND = 404
    CONTENT_TOO_LARGE = 413
    SERVER_ERROR = 500


def validate_positive_int(value: object) -> int:
    try:
        ivalue: int = int(value)  # type: ignore[call-overload]
    except (TypeError, ValueError):
        raise ValueError(f"value is not an integer, got {value!r}")
    if ivalue <= 0:
        raise ValueError(f"value must be greater than 0, got {value!r}")
    return ivalue


def validate_positive_double(value: object) -> float:
    try:
        fvalue = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        raise ValueError(f"value is not a number, got {value!r}")
    if fvalue <= 0:
        raise ValueError(f"value must be greater than 0, got {value!r}")
    return fvalue


def make_prefix_formatter(prefix: str) -> logging.Formatter:
    """Return a Formatter that places *prefix* before the timestamp."""
    _BASE_FMT = "%(asctime)s %(levelname)s %(filename)s:%(lineno)d %(message)s"
    return logging.Formatter(f"{prefix} {_BASE_FMT}")


def prefix_line(text: str, prefix: str) -> str:
    """Prepend *prefix* to each line in *text*, preserving tqdm's \\r in-place updates."""
    if not text or text.isspace():
        return text
    if text.startswith("\r") and "\n" not in text:
        return f"\r{prefix} {text[1:]}"
    if text.endswith("\r"):
        return f"\r{prefix} {text[:-1]}"
    lines = text.splitlines(keepends=True)
    return "".join(f"{prefix} {line}" if line.strip() else line for line in lines)
