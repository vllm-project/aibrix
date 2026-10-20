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

import pytest

from aibrix.batch.manifest.engine_adapter import build_engine_args
from aibrix.batch.template import ModelDeploymentTemplateSpec


def template(engine="vllm", version="0.32.0", **changes):
    return ModelDeploymentTemplateSpec.model_validate(
        {
            "engine": {"type": engine, "version": version, "image": engine},
            "model_source": {"type": "huggingface", "uri": "Qwen/Qwen2.5-7B"},
            "accelerator": {"type": "H100", "count": 1},
            "supported_endpoints": ["/v1/chat/completions"],
            **changes,
        }
    )


def test_sglang_pipeline_parallelism_uses_unambiguous_flag():
    args = build_engine_args(
        template(
            "sglang",
            "0.5.22",
            parallelism={"pp": 2},
            accelerator={"type": "H100", "count": 2},
        )
    )
    assert args[args.index("--pipeline-parallel-size") + 1] == "2"
    assert "--pp" not in args


def test_sglang_block_size_maps_to_page_size_once():
    args = build_engine_args(
        template("sglang", "0.5.22", engine_args={"block_size": 16})
    )
    assert args[args.index("--page-size") + 1] == "16"
    assert args.count("--page-size") == 1
    assert "--block-size" not in args


@pytest.mark.parametrize("weight,dtype", [("bf16", "bfloat16"), ("fp16", "float16")])
def test_vllm_unquantized_precision_uses_dtype(weight, dtype):
    args = build_engine_args(template(quantization={"weight": weight}))
    assert args[args.index("--dtype") + 1] == dtype
    assert "--quantization" not in args


@pytest.mark.parametrize("weight", ["fp8", "awq", "gptq", "int8"])
def test_vllm_quantization_mapping_is_preserved(weight):
    args = build_engine_args(template(quantization={"weight": weight}))
    assert args[args.index("--quantization") + 1] == weight


@pytest.mark.parametrize(
    "version",
    ["0.18.0", "0.32.0", "v0.32.0", "0.32.0+vendor", "0.18-rc1", "0.18a", "0.18.0rc1"],
)
@pytest.mark.parametrize("swap_space", [0, 4])
def test_vllm_removed_swap_space_fails_before_deployment(version, swap_space):
    with pytest.raises(ValueError, match="swap_space.*vLLM"):
        build_engine_args(
            template(version=version, engine_args={"swap_space": swap_space})
        )


@pytest.mark.parametrize("version", ["0.6.3", "0.17.0", "0.17-rc1"])
def test_vllm_legacy_swap_space_is_preserved(version):
    args = build_engine_args(template(version=version, engine_args={"swap_space": 4}))
    assert args[args.index("--swap-space") + 1] == "4"


def test_vllm_modern_default_omits_swap_space():
    assert "--swap-space" not in build_engine_args(template())
