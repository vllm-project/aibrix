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

import numpy as np

from aibrix.gpu_optimizer.optimizer.profiling.gpu_benchmark import load_response


class TestGenBenchmark(unittest.TestCase):
    def __init__(self, methodName: str = "runTest") -> None:
        super().__init__(methodName)

    def test_stair_agggregate(self):
        resp = '{"message": "\\n\\"test\\""}'
        expected = {"message": '\n"test"'}

        np.testing.assert_equal(load_response(resp), expected)


if __name__ == "__main__":
    unittest.main()
