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

from abc import abstractmethod
from typing import Generic, TypeVar

V = TypeVar("V")
R = TypeVar("R")


class Marshaller(Generic[V, R]):
    """Marshaller is an abstraction of serializer, compressor, etc."""

    @abstractmethod
    def marshal(self, data: V) -> R:
        raise NotImplementedError

    @abstractmethod
    def unmarshal(self, data: R) -> V:
        raise NotImplementedError


class BaseMarshaller(Marshaller[V, R]):
    def __init__(self, marshaller: Marshaller | None = None) -> None:
        self._marshaller = marshaller

    def marshal(self, data: V) -> R:
        if self._marshaller is None:
            return self._marshal(data)
        else:
            return self._marshal(self._marshaller.marshal(data))

    def unmarshal(self, data: R) -> V:
        if self._marshaller is None:
            return self._unmarshal(data)
        else:
            return self._marshaller.unmarshal(self._unmarshal(data))

    @abstractmethod
    def _marshal(self, data: V) -> R:
        raise NotImplementedError

    @abstractmethod
    def _unmarshal(self, data: R) -> V:
        raise NotImplementedError
