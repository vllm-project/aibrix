# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass
from time import perf_counter
from typing import Iterable


@dataclass(frozen=True)
class Tag:
    name: str
    value: str


def T(name: str, value: str) -> Tag:
    return Tag(name=name, value=value)


def normalize_tags(tags: Iterable[Tag]) -> tuple[Tag, ...]:
    deduped: dict[str, str] = {}
    for tag in tags:
        deduped[tag.name] = tag.value
    return tuple(Tag(name=name, value=deduped[name]) for name in sorted(deduped))


class Sink(ABC):
    @abstractmethod
    def counter(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def timer(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def store(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def rate(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def close(self) -> None:
        raise NotImplementedError


class NoopSink(Sink):
    def counter(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def store(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def close(self) -> None:
        return None


class FanoutSink(Sink):
    def __init__(self, sinks: Iterable[Sink]):
        self._sinks = tuple(sinks)

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.counter(name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.gauge(name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.timer(name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.store(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.rate(name, value, *tags)

    def close(self) -> None:
        for sink in self._sinks:
            sink.close()


class GlobalEmitter(Sink):
    def __init__(self) -> None:
        self._sink: Sink = NoopSink()

    def set_sink(self, sink: Sink) -> None:
        self._sink = sink

    def reset(self) -> None:
        self._sink = NoopSink()

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.counter(name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.gauge(name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.timer(name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.store(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.rate(name, value, *tags)

    def close(self) -> None:
        self._sink.close()


Emitter = GlobalEmitter()


def duration_ms(sink: Sink, name: str, start_time: float, *tags: Tag) -> None:
    sink.timer(f"{name}.ms", (perf_counter() - start_time) * 1000.0, *tags)
