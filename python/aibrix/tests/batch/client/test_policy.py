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

from aibrix.batch.client.policy import RoundRobin


def test_round_robin_returns_none_when_no_channels():
    router = RoundRobin()
    assert router.pick(None, []) is None


def test_single_channel_always_returns_that_channel():
    router = RoundRobin()
    assert router.pick(None, ["s"]) == "s"
    assert router.pick(None, ["s"]) == "s"
    assert router.pick(None, ["s"]) == "s"


def test_multiple_channels_rotate_in_order_and_wrap_around():
    # The fourth pick returning "a" again is the point: it shows the cursor is
    # taken modulo the channel count rather than growing without bound.
    router = RoundRobin()
    channels = ["a", "b", "c"]
    assert router.pick(None, channels) == "a"
    assert router.pick(None, channels) == "b"
    assert router.pick(None, channels) == "c"
    assert router.pick(None, channels) == "a"
