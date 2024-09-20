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
import pytest

from aibrix.metrics.engine_rules import get_metric_standard_rules


def test_get_metric_standard_rules_ignore_case():
    # Engine str is all lowercase
    rules = get_metric_standard_rules("vllm")
    assert rules is not None

    # The function get_metric_standard_rules is case-insensitive
    rules2 = get_metric_standard_rules("vLLM")
    assert rules == rules2


def test_get_metric_standard_rules_not_support():
    # SGLang and TensorRT-LLM are not supported
    with pytest.raises(ValueError):
        get_metric_standard_rules("SGLang")

    with pytest.raises(ValueError):
        get_metric_standard_rules("TensorRT-LLM")
