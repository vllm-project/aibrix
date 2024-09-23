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

from abc import ABC, abstractmethod
from dataclasses import dataclass

from packaging.version import Version


@dataclass
class InferenceEngine(ABC):
    """Base class for Inference Engine."""

    name: str
    version: str
    endpoint: str

    @abstractmethod
    def load_lora_adapter(self):
        pass

    @abstractmethod
    def unload_lora_adapter(self):
        pass


def get_inference_engine(engine: str, version: str, endpoint: str) -> InferenceEngine:
    if engine.lower() == "vllm":
        # Not support lora dynamic loading & unloading
        if Version(version) < Version("0.6.1"):
            from aibrix.openapi.engine.vllm import VLLMInferenceEngine

            return VLLMInferenceEngine(engine, version, endpoint)
        else:
            from aibrix.openapi.engine.vllm import VLLMInferenceEngine

            return VLLMInferenceEngine(engine, version, endpoint)
    else:
        raise ValueError(f"Engine {engine} with version {version} is not supported.")
