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

import copy
import json
import time

import pytest

from aibrix_kvcache.l2.connectors import ConnectorConfig
from aibrix_kvcache.l2.placement import Placement, PlacementConfig
from aibrix_kvcache.meta_service import MetaServiceConfig, RedisMetaService


@pytest.fixture
def simple_cluster_data():
    return {
        "nodes": [
            {
                "addr": "10.0.0.1",
                "port": 8000,
                "slots": [{"start": 0, "end": 511}],
            },
            {
                "addr": "10.0.0.2",
                "port": 8000,
                "slots": [{"start": 512, "end": 1023}],
            },
        ]
    }


@pytest.fixture
def complex_cluster_data():
    return {
        "nodes": [
            {
                "addr": "10.0.0.1",
                "port": 8000,
                "slots": [{"start": 0, "end": 100}, {"start": 200, "end": 300}],
            },
            {
                "addr": "10.0.0.2",
                "port": 8000,
                "slots": [
                    {"start": 101, "end": 199},
                    {"start": 301, "end": 400},
                ],
            },
        ]
    }


PLACEMENT_CONFIG = PlacementConfig(
    placement_policy="SIMPLE",
    conn_config=ConnectorConfig(
        backend_name="MOCK",
        namespace="test_namespace",
        partition_id="test_partition",
        executor=None,
    ),
)


def test_invalid_json():
    placement = Placement.create(config=PLACEMENT_CONFIG)
    status = placement.construct_cluster("invalid json")
    assert status.is_invalid()


def test_missing_fields():
    placement = Placement.create(config=PLACEMENT_CONFIG)
    bad_data = {"nodes": [{"addr": "10.0.0.1"}]}  # Missing port and slots
    status = placement.construct_cluster(json.dumps(bad_data))
    assert status.is_invalid()


def test_construct_simple_cluster(simple_cluster_data):
    placement = Placement.create(config=PLACEMENT_CONFIG)
    placement.construct_cluster(json.dumps(simple_cluster_data))

    assert len(placement.members) == 2
    assert len(placement.slots) == 1024

    # Verify members
    members = placement.members
    assert members[0].meta["addr"] == "10.0.0.1"
    assert members[0].meta["port"] == 8000
    assert members[1].meta["addr"] == "10.0.0.2"
    assert members[1].meta["port"] == 8000

    # Verify slot assignments
    assert placement.slots[0][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[511][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[512][1].meta["addr"] == "10.0.0.2"
    assert placement.slots[1023][1].meta["addr"] == "10.0.0.2"


def test_construct_complex_cluster(complex_cluster_data):
    placement = Placement.create(config=PLACEMENT_CONFIG)
    placement.construct_cluster(json.dumps(complex_cluster_data))

    assert len(placement.members) == 2
    assert len(placement.slots) == 401

    # Test boundary slots
    assert placement.slots[0][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[100][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[101][1].meta["addr"] == "10.0.0.2"
    assert placement.slots[199][1].meta["addr"] == "10.0.0.2"
    assert placement.slots[200][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[300][1].meta["addr"] == "10.0.0.1"
    assert placement.slots[301][1].meta["addr"] == "10.0.0.2"
    assert placement.slots[400][1].meta["addr"] == "10.0.0.2"


def test_select(simple_cluster_data):
    placement = Placement.create(config=PLACEMENT_CONFIG)
    placement.construct_cluster(json.dumps(simple_cluster_data))

    # Test keys that should hash to different slots
    test_cases = {
        "key1": (0, "10.0.0.1"),
        "key2": (511, "10.0.0.1"),
        "a_long_key": (512, "10.0.0.2"),
        "12345": (100, "10.0.0.1"),
        "special!chars#key": (1023, "10.0.0.2"),
    }

    placement.hash_fn = lambda x: test_cases[x][0]

    for key, value in test_cases.items():
        _, expected_addr = value
        status = placement.select(key)
        assert status.is_ok()
        member = status.get()
        assert member.meta["addr"] == expected_addr


def test_background_refresh(redis_server, redis_client):
    """Test that background refresh works correctly."""
    # Initial test data
    initial_data = {
        "nodes": [
            {
                "addr": "initial.host",
                "port": 1234,
                "slots": [{"start": 0, "end": 100}],
            }
        ]
    }

    redis_key = "test_key"
    redis_host, redis_port = redis_server
    redis_url = f"redis://{redis_host}:{redis_port}"
    config = MetaServiceConfig(
        url=redis_url,
        cluster_meta_key=redis_key,
    )
    redis_ms = RedisMetaService(config)
    redis_ms.open()

    # Store initial data in Redis
    redis_client.set(redis_key, json.dumps(initial_data))

    config = copy.deepcopy(PLACEMENT_CONFIG)
    config.meta_service = redis_ms
    config.refresh_interval_s = 2
    placement = Placement.create(config=config)
    placement.open()

    # Verify initial state
    assert len(placement.members) == 1
    assert placement.members[0].meta["addr"] == "initial.host"
    assert placement.members[0].meta["port"] == 1234

    # Update the data in Redis
    updated_data = {
        "nodes": [
            {
                "addr": "updated.host",
                "port": 5678,
                "slots": [{"start": 0, "end": 200}],
            }
        ]
    }
    redis_client.set(redis_key, json.dumps(updated_data))

    # Wait for refresh to pick up changes (should happen within 4 seconds)
    time.sleep(4)

    # Verify refresh happened
    assert placement.members[0].meta["addr"] == "updated.host"
    assert placement.members[0].meta["port"] == 5678

    placement.close()
    redis_ms.close()
