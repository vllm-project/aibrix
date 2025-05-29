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
import functools
import traceback
from dataclasses import dataclass
from enum import Enum, auto
from typing import Any, Generic, TypeVar

T = TypeVar("T")
U = TypeVar("U")


class StatusCodes(Enum):
    OK = auto()
    ERROR = auto()
    NOT_FOUND = auto()
    INVALID = auto()
    OUT_OF_MEMORY = auto()
    TIMEOUT = auto()
    DENIED = auto()
    CANCELLED = auto()


@dataclass
class Status(Generic[T]):
    """A generic status container that can represent either success (with a
    value) or failure (with an error code and optional message/exception).
    """

    error_code: StatusCodes
    value: T | str | Exception | None
    trace: str | None = None

    def __init__(  # type: ignore[misc]
        self,
        error_code_or_value: StatusCodes | T | "Status[Any]",
        value: T | str | Exception | None = None,
    ) -> None:
        if isinstance(error_code_or_value, Status):
            # Conversion case from another Status
            other = error_code_or_value
            self.error_code = other.error_code
            self.value = other.value
            self.trace = other.trace
        elif isinstance(error_code_or_value, StatusCodes):
            if error_code_or_value == StatusCodes.OK:
                self.error_code = StatusCodes.OK
                self.value = value
            else:
                # Error case
                self.error_code = error_code_or_value
                self.value = value
                if isinstance(value, Exception):
                    self.trace = traceback.format_exc()
        else:
            # Success case
            assert value is None
            self.error_code = StatusCodes.OK
            self.value = error_code_or_value

    def is_ok(self) -> bool:
        return self.error_code == StatusCodes.OK

    def get(self, default=None) -> T:
        """Returns the value if successful, otherwise returns default."""
        if not self.is_ok():
            return default

        assert self.value is not None, "Successful status had None value"
        return self.value  # type: ignore

    @classmethod
    def ok(cls, value: T | None = None) -> "Status[T]":
        """Factory method for success status."""
        return cls(StatusCodes.OK, value)

    @classmethod
    def error(
        cls,
        error_code: StatusCodes,
        message_or_exception: str | Exception | None = None,
    ) -> "Status[T]":
        """Factory method for error status."""
        return cls(error_code, message_or_exception)

    def __getattr__(self, name):
        if name.startswith("is_"):
            expected_code = name[3:].upper()
            return lambda: self.error_code == getattr(
                StatusCodes, expected_code, None
            )
        raise AttributeError(f"{name} not found")

    def __repr__(self):
        if self.trace is not None:
            return (
                f"Status(error_code={self.error_code}, value={self.value}),"
                f" trace:\n{self.trace}"
            )
        elif self.value is not None:
            return f"Status(error_code={self.error_code}, value={self.value})"
        else:
            return f"Status(error_code={self.error_code})"

    def __str__(self) -> str:
        return self.__repr__()

    def raise_if_has_exception(self) -> None:
        if self.value is not None and isinstance(self.value, Exception):
            raise self.value

    def raise_if_not_ok(self) -> None:
        self.raise_if_has_exception()
        if not self.is_ok():
            raise RuntimeError(f"{self}")

    @staticmethod
    def capture_exception(func):
        """A decorator that converts exceptions raised by the decorated
        function into Status.
        """

        @functools.wraps(func)
        def wrapper(*args, **kwargs):
            try:
                return func(*args, **kwargs)
            except Exception as e:
                return Status(StatusCodes.ERROR, e)

        @functools.wraps(func)
        async def async_wrapper(*args, **kwargs):
            try:
                return await func(*args, **kwargs)
            except Exception as e:
                return Status(StatusCodes.ERROR, e)

        if asyncio.iscoroutinefunction(func):
            return async_wrapper
        else:
            return wrapper
