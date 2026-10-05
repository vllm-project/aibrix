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

from __future__ import annotations

from typing import Optional


class FakeRedisClient:
    """Shared in-memory async Redis fake for unit tests."""

    def __init__(
        self,
        *,
        values: Optional[dict[str, bytes]] = None,
        objects: Optional[dict[str, bytes]] = None,
        sets: Optional[dict[str, set[str]]] = None,
        zsets: Optional[dict[str, dict[str, float]]] = None,
        existing_keys: Optional[set[str]] = None,
        ping_result: bool = True,
    ) -> None:
        initial_objects = dict(objects or values or {})
        self.objects: dict[str, bytes] = initial_objects
        self.values: dict[str, bytes] = dict(values or initial_objects)
        self.sets: dict[str, set[str]] = {
            key: set(val) for key, val in (sets or {}).items()
        }
        self.zsets: dict[str, dict[str, float]] = {
            key: {member: float(score) for member, score in members.items()}
            for key, members in (zsets or {}).items()
        }
        self.existing_keys: set[str] = set(existing_keys or set())
        self.ping_result = ping_result
        self.zscore_calls = 0

    @classmethod
    def from_created_at_asc(
        cls, keys_in_created_at_asc: list[str]
    ) -> "FakeRedisClient":
        return cls(
            zsets={
                "timestamps:all": {
                    key: float(index)
                    for index, key in enumerate(keys_in_created_at_asc, start=1)
                }
            }
        )

    def with_hierarchical_index(
        self, prefix: str, members_in_created_at_asc: list[str]
    ) -> "FakeRedisClient":
        self.existing_keys.add(f"{prefix}:index")
        self.existing_keys.add(f"timestamps:{prefix}")
        self.zsets[f"timestamps:{prefix}"] = {
            member: float(index)
            for index, member in enumerate(members_in_created_at_asc, start=1)
        }
        return self

    async def ping(self):
        return self.ping_result

    async def get(self, key: str):
        return self.values.get(key)

    async def set(
        self,
        key: str,
        value,
        ex=None,
        px=None,
        nx: bool = False,
        xx: bool = False,
    ):
        del ex, px
        if nx and key in self.objects:
            return None
        if xx and key not in self.objects:
            return None
        if isinstance(value, str):
            value = value.encode("utf-8")
        self.objects[key] = value
        self.values[key] = value
        self.existing_keys.add(key)
        return True

    async def delete(self, *names: str):
        removed = 0
        for name in names:
            removed += int(self.objects.pop(name, None) is not None)
            self.values.pop(name, None)
            self.zsets.pop(name, None)
            self.sets.pop(name, None)
            self.existing_keys.discard(name)
        return removed

    async def sadd(self, key: str, *members: str):
        existing_members = self.sets.setdefault(key, set())
        size_before = len(existing_members)
        existing_members.update(members)
        self.existing_keys.add(key)
        return len(existing_members) - size_before

    async def srem(self, key: str, *values: str):
        members = self.sets.get(key, set())
        removed = 0
        for value in values:
            if value in members:
                members.remove(value)
                removed += 1
        return removed

    async def smembers(self, key: str):
        return {member.encode("utf-8") for member in self.sets.get(key, set())}

    async def zadd(
        self, key: str, mapping: dict[str, float], nx: bool = False, **kwargs
    ):
        del kwargs
        zset = self.zsets.setdefault(key, {})
        added = 0
        for member, score in mapping.items():
            if nx and member in zset:
                continue
            if member not in zset:
                added += 1
            zset[member] = float(score)
        self.existing_keys.add(key)
        return added

    async def zscore(self, key: str, member: str):
        self.zscore_calls += 1
        return self.zsets.get(key, {}).get(member)

    async def zrem(self, key: str, *values: str):
        zset = self.zsets.get(key, {})
        removed = 0
        for value in values:
            removed += int(zset.pop(value, None) is not None)
        return removed

    async def zrange(self, key: str, start: int, end: int, withscores: bool = False):
        return self._zrange(key, start, end, reverse=False, withscores=withscores)

    async def zrevrange(
        self,
        key: str,
        start: int,
        end: int,
        withscores: bool = False,
        score_cast_func=float,
    ):
        del score_cast_func
        return self._zrange(key, start, end, reverse=True, withscores=withscores)

    def _zrange(self, key: str, start: int, end: int, reverse: bool, withscores: bool):
        members = sorted(
            self.zsets.get(key, {}).items(),
            key=lambda item: item[1],
            reverse=reverse,
        )
        if end == -1:
            sliced = members[start:]
        else:
            sliced = members[start : end + 1]
        if withscores:
            return [(member.encode("utf-8"), score) for member, score in sliced]
        return [member.encode("utf-8") for member, _ in sliced]

    async def zrank(self, key: str, member: str):
        return self._zrank(key, member, reverse=False)

    async def zrevrank(self, key: str, member: str):
        return self._zrank(key, member, reverse=True)

    def _zrank(self, key: str, member: str, reverse: bool):
        ordered_members = [
            name
            for name, _ in sorted(
                self.zsets.get(key, {}).items(),
                key=lambda item: item[1],
                reverse=reverse,
            )
        ]
        try:
            return ordered_members.index(member)
        except ValueError:
            return None

    async def exists(self, key: str) -> bool:
        return (
            key in self.existing_keys
            or key in self.objects
            or key in self.values
            or key in self.zsets
            or key in self.sets
        )

    async def strlen(self, key: str) -> int:
        value = self.values.get(key)
        return len(value) if value is not None else 0

    async def aclose(self) -> None:
        return None
