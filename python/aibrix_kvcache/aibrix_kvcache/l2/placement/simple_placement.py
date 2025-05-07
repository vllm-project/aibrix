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
from typing import TypeVar

from farmhash import FarmHash64

from ...status import Status
from .placement import BasePlacement, Member, PlacementConfig

K = TypeVar("K")
V = TypeVar("V")


class SimplePlacement(BasePlacement[K, V]):
    def __init__(self, *, config: PlacementConfig):
        super().__init__(config=config)
        self.hash_fn = FarmHash64

    def select(self, key: K) -> Status[Member]:
        """Select a member for the given key"""
        slot = self.hash_fn(key) % self.total_slots
        member = self.slots[slot][1]
        return Status.ok(member)
