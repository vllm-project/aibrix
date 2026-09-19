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

from unittest.mock import MagicMock

from aibrix.batch.client.channel import Channel
from aibrix.batch.client.policy import RoundRobin


def test_round_robin_returns_none_when_no_channels():
    router = RoundRobin()
    assert router.pick(MagicMock(), []) is None


def test_single_channel_always_returns_that_channel():
    router = RoundRobin()
    channel = MagicMock()
    assert router.pick(MagicMock(), [channel]) is channel
    assert router.pick(MagicMock(), [channel]) is channel
    assert router.pick(MagicMock(), [channel]) is channel


def test_multiple_channels_rotate_in_order_and_wrap_around():
    # The fourth pick returning the first channel again is the point: it shows
    # the cursor is taken modulo the channel count rather than growing forever.
    router = RoundRobin()
    # Annotated so the list is typed as list[Channel]; a bare list literal
    # assigned to a variable would be inferred as list[MagicMock], and list is
    # invariant, so it would not satisfy pick's signature under strict mypy.
    channels: list[Channel] = [MagicMock(), MagicMock(), MagicMock()]
    assert router.pick(MagicMock(), channels) is channels[0]
    assert router.pick(MagicMock(), channels) is channels[1]
    assert router.pick(MagicMock(), channels) is channels[2]
    assert router.pick(MagicMock(), channels) is channels[0]


def test_cursor_survives_the_channel_set_growing_and_shrinking():
    # The docstring promises the cursor tolerates the reachable set changing
    # between calls, because it is taken modulo the length that is live at the
    # time of the call. Offer a different-sized list on each pick to check that.
    # Cursor starts at 0: 0 % 3 picks the first and leaves the cursor at 1;
    # 1 % 2 picks the second and leaves it at 0; 0 % 1 picks the first again.
    router = RoundRobin()
    first, second, third = MagicMock(), MagicMock(), MagicMock()
    assert router.pick(MagicMock(), [first, second, third]) is first
    assert router.pick(MagicMock(), [first, second]) is second
    assert router.pick(MagicMock(), [first]) is first
