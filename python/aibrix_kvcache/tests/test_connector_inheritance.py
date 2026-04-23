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

"""Lightweight asserts that the Type2 Connector correctly inherits from
Type1 and that the shared ``__init__`` wires the right Scheduler/Worker
subclass through the ``SCHEDULER_CLASS`` / ``WORKER_CLASS`` class
attributes.

No vLLM runtime / GPU dependency at test time — we only inspect class
attributes, not runtime instances. Catches regressions where someone
shadows ``SCHEDULER_CLASS`` / ``WORKER_CLASS`` with the wrong type, or
breaks the Connector / Scheduler / Worker hierarchy.
"""

from aibrix_kvcache.integration.vllm.kv_connector import (
    aibrix_offloading_connector_type1 as t1,
)
from aibrix_kvcache.integration.vllm.kv_connector import (
    aibrix_offloading_connector_type2 as t2,
)


def test_type1_class_attrs_point_to_type1_subclasses():
    assert (
        t1.AIBrixOffloadingConnector.SCHEDULER_CLASS
        is t1.AIBrixOffloadingConnectorScheduler
    )
    assert (
        t1.AIBrixOffloadingConnector.WORKER_CLASS
        is t1.AIBrixOffloadingConnectorWorker
    )


def test_type2_overrides_class_attrs_to_type2_subclasses():
    assert (
        t2.AIBrixOffloadingConnector.SCHEDULER_CLASS
        is t2.AIBrixOffloadingConnectorScheduler
    )
    assert (
        t2.AIBrixOffloadingConnector.WORKER_CLASS
        is t2.AIBrixOffloadingConnectorWorker
    )
    # Must NOT still point at the Type1 subclasses.
    assert (
        t2.AIBrixOffloadingConnector.SCHEDULER_CLASS
        is not t1.AIBrixOffloadingConnectorScheduler
    )
    assert (
        t2.AIBrixOffloadingConnector.WORKER_CLASS
        is not t1.AIBrixOffloadingConnectorWorker
    )


def test_type2_inherits_from_type1():
    assert issubclass(
        t2.AIBrixOffloadingConnector, t1.AIBrixOffloadingConnector
    )
    assert issubclass(
        t2.AIBrixOffloadingConnectorScheduler,
        t1.AIBrixOffloadingConnectorScheduler,
    )
    assert issubclass(
        t2.AIBrixOffloadingConnectorWorker,
        t1.AIBrixOffloadingConnectorWorker,
    )
