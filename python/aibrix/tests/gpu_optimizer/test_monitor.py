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
import unittest

from aibrix.gpu_optimizer.load_monitor.monitor import (
    DeploymentStates,
    ModelMonitor,
)
from aibrix.gpu_optimizer.optimizer import GPUProfile


class ProfileReader:
    def __init__(self, cost: float, created: float):
        self.set(cost, created)

    def set(self, cost: float, created: float):
        # gen_profile names the profile after the deployment, without namespace
        self.profile = dict(
            gpu="llama-a100",
            cost=cost,
            tputs=[[5.0]],
            indexes=[[64], [128]],
            created=created,
        )

    def read(self):
        return [GPUProfile(**self.profile)]


class NoLoadReader:
    def read(self, ts=0.0):
        return [], 0.0

    def progress(self):
        return ""

    def next_available(self):
        return 0.0


class TestModelMonitorProfiles(unittest.TestCase):
    def assert_cost(self, monitor: ModelMonitor, cost: float):
        profile = monitor.deployments["default/llama-a100"].profile
        assert profile is not None
        self.assertEqual(profile.cost, cost)
        gpu_info = monitor._optimizer._config.gpu_info
        self.assertEqual(gpu_info["default/llama-a100"]["cost"], cost)

    def test_updated_profile_loaded_before_deployment(self):
        # Order used by the optimizer app: profiles, then the deployment
        reader = ProfileReader(cost=1.0, created=100.0)
        monitor = ModelMonitor(
            "llama",
            "1",
            NoLoadReader(),
            deployment=DeploymentStates("llama-a100", "default", 2),
            namespace="default",
            profile_reader=reader,
            gpu_fraction=1.0,
        )
        self.assert_cost(monitor, 1.0)
        self.assertEqual(monitor._cost, 1.0 * 2)

        # Regenerated profile for the same deployment with a new cost
        reader.set(cost=3.0, created=200.0)
        self.assertTrue(monitor.load_profiles())
        self.assert_cost(monitor, 3.0)
        self.assertEqual(monitor._cost, 3.0 * 2)

    def test_updated_profile_loaded_after_deployment(self):
        monitor = ModelMonitor(
            "llama",
            "1",
            NoLoadReader(),
            deployment=DeploymentStates("llama-a100", "default", 2),
            namespace="default",
            gpu_fraction=1.0,
        )
        reader = ProfileReader(cost=1.0, created=100.0)
        self.assertTrue(monitor.load_profiles(reader))
        self.assert_cost(monitor, 1.0)

        reader.set(cost=3.0, created=200.0)
        self.assertTrue(monitor.load_profiles())
        self.assert_cost(monitor, 3.0)


if __name__ == "__main__":
    unittest.main()
