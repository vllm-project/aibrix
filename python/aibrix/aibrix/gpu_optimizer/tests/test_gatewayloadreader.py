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
import json
import unittest

import numpy as np

from aibrix.gpu_optimizer.load_monitor.load_reader import (
    GatewayLoadReader,
)


class TestDatasetLoadReader(unittest.TestCase):
    def __init__(self, methodName: str = "runTest") -> None:
        super().__init__(methodName)

        self.reader = GatewayLoadReader(None, "test_model")  # type: ignore

    def test_stair_agggregate(self):
        load_profile = '{"103:46":1,"103:50":1,"103:56":1,"103:60":1,"103:66":1,"104:53":1,"104:54":1,"105:60":1,"113:57":1,"113:59":1,"119:57":1,"119:59":1,"120:56":1,"120:58":1,"120:60":3,"120:62":2,"121:56":2,"121:60":1,"125:38":1,"125:53":2,"125:54":1,"126:52":3,"126:55":1,"126:56":1,"126:60":1,"126:61":1,"126:63":1,"126:68":1,"127:53":1,"127:57":1,"130:40":1,"130:49":1,"130:56":1,"79:46":1,"79:48":2,"79:49":2,"79:50":1,"79:52":2,"80:46":1,"80:48":1,"80:49":2,"80:50":2,"80:51":1,"80:52":4,"81:48":1,"81:49":1,"81:52":2,"82:50":1,"82:51":1,"83:10":1,"83:47":1,"83:48":3,"83:49":2,"83:52":1,"84:48":2,"84:49":1,"84:50":2,"84:52":1,"85:47":1,"85:49":1,"85:51":2,"86:51":1,"87:49":1,"meta_interval_sec":10,"meta_precision":10,"meta_v":2}'
        ts = 1735693670.0

        profile = json.loads(load_profile)

        records = self.reader._parse_profiles(profile, ts)

        np.testing.assert_equal(len(records), 63)
        sum_count = np.sum([record.freq for record in records])
        np.testing.assert_equal(sum_count, 85)

        # A bug that read twice willout out_record input will add up old records
        records2 = self.reader._parse_profiles(profile, ts)
        np.testing.assert_equal(len(records2), 63)


if __name__ == "__main__":
    unittest.main()
