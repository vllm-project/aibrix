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

"""Engine-specific argument generation.

Each engine type (vLLM, SGLang, TRT-LLM, mock) has different
command-line conventions. This module dispatches by EngineType and
returns the container args list that the renderer drops into the
engine container spec.

At this moment, it implements:
- VLLM: full coverage of common flags
- SGLANG: minimal OpenAI-compatible server flags
- MOCK: minimal startup (used by integration tests)

Other engine types raise UnsupportedEngineError until their adapter
is implemented in later phases.
"""

from typing import Any, Dict, List, Optional

from aibrix.batch.template import (
    EngineSpec,
    EngineType,
    ModelDeploymentTemplateSpec,
)


class UnsupportedEngineError(ValueError):
    """Raised when a template specifies an engine type whose adapter
    has not been implemented yet."""


def build_engine_args(
    spec: ModelDeploymentTemplateSpec,
    ignore_model: bool = False,
    served_model_name: Optional[str] = None,
) -> List[str]:
    """Dispatch to the engine-specific argument builder.

    Args:
        spec: The full template spec.
        served_model_name: Serving identifier batch requests carry in
            body.model (spec.aibrix.model). Pinned via the engine's
            --served-model-name so the served name doesn't silently
            default to the weights path; a template that sets the flag
            itself (engine_args or serve_args) wins.

    Returns:
        List of CLI args (or shell-script lines for shell-mode engines)
        ready to be set as the container's args.

    Raises:
        UnsupportedEngineError: If engine.type has no adapter yet.
    """
    engine_type = spec.engine.type
    if engine_type == EngineType.VLLM:
        return _build_vllm_args(spec, ignore_model, served_model_name)
    if engine_type == EngineType.SGLANG:
        return _build_sglang_args(spec, ignore_model, served_model_name)
    if engine_type == EngineType.MOCK:
        return _build_mock_args(spec)
    if engine_type == EngineType.VIPE:
        return _build_vipe_args(spec, ignore_model, served_model_name)
    raise UnsupportedEngineError(
        f"engine type '{engine_type.value}' has no adapter yet; supported: vllm, sglang, mock, vipe"
    )


def _build_vllm_args(
    spec: ModelDeploymentTemplateSpec,
    ignore_model: bool = False,
    served_model_name: Optional[str] = None,
) -> List[str]:
    """Render vLLM `vllm serve` (or `python -m vllm.entrypoints.openai.api_server`) arguments.

    Order of fields follows vLLM's CLI convention: model first, then
    parallelism, then engine_args, then quantization, then admin-supplied
    serve_args last (so admin overrides win).
    """
    args: List[str] = []

    # 1. Model
    if not ignore_model:
        args.extend(["--model", spec.model_source.uri])
    if served_model_name and not _pins_served_model_name(spec):
        args.extend(["--served-model-name", served_model_name])
    if spec.model_source.revision:
        args.extend(["--revision", spec.model_source.revision])
    if spec.model_source.tokenizer_path:
        args.extend(["--tokenizer", spec.model_source.tokenizer_path])
    if spec.model_source.chat_template_path:
        args.extend(["--chat-template", spec.model_source.chat_template_path])

    # 2. Parallelism (only emit non-default values to keep arg list lean)
    p = spec.parallelism
    if p.tp > 1:
        args.extend(["--tensor-parallel-size", str(p.tp)])
    if p.pp > 1:
        args.extend(["--pipeline-parallel-size", str(p.pp)])
    if p.dp > 1:
        # vLLM 0.6+ supports --data-parallel-size on some distributed setups
        args.extend(["--data-parallel-size", str(p.dp)])

    # 3. Engine args (typed fields)
    ea = spec.engine_args
    if ea.max_num_batched_tokens is not None:
        args.extend(["--max-num-batched-tokens", str(ea.max_num_batched_tokens)])
    if ea.max_num_seqs is not None:
        args.extend(["--max-num-seqs", str(ea.max_num_seqs)])
    if ea.max_model_len is not None:
        args.extend(["--max-model-len", str(ea.max_model_len)])
    if ea.gpu_memory_utilization is not None:
        args.extend(["--gpu-memory-utilization", str(ea.gpu_memory_utilization)])
    if ea.block_size is not None:
        args.extend(["--block-size", str(ea.block_size)])
    if ea.swap_space is not None:
        args.extend(["--swap-space", str(ea.swap_space)])
    if ea.enable_prefix_caching:
        args.append("--enable-prefix-caching")
    if ea.enable_chunked_prefill:
        args.append("--enable-chunked-prefill")
    if ea.speculative_model:
        args.extend(["--speculative-model", ea.speculative_model])
        if ea.num_speculative_tokens:
            args.extend(["--num-speculative-tokens", str(ea.num_speculative_tokens)])

    # Engine-specific extras (lenient model_config='allow' captures these)
    extras = _engine_args_extras(ea.model_dump(exclude_none=True))
    for key, value in sorted(extras.items()):
        flag = "--" + key.strip().lstrip("-").replace("_", "-")
        if isinstance(value, bool):
            if value:
                args.append(flag)
        elif isinstance(value, list):
            for v in value:
                args.extend([flag, str(v)])
        elif value is None or str(value).strip() == "":
            args.append(flag)
        else:
            args.extend([flag, str(value).strip()])

    # 4. Quantization
    q = spec.quantization
    if q.weight is not None:
        args.extend(["--quantization", q.weight.value])
    if q.kv_cache is not None:
        args.extend(["--kv-cache-dtype", q.kv_cache.value])

    # 5. Admin-supplied serve_args (raw, last, can override anything above)
    args.extend(spec.engine.serve_args)

    return args


# Typed fields of EngineArgsSpec; everything else is forwarded as
# engine-specific extras.
_VLLM_TYPED_FIELDS = {
    "max_num_batched_tokens",
    "max_num_seqs",
    "max_model_len",
    "gpu_memory_utilization",
    "block_size",
    "swap_space",
    "enable_prefix_caching",
    "enable_chunked_prefill",
    "speculative_model",
    "num_speculative_tokens",
}

_SGLANG_TYPED_FIELDS = {
    "max_num_seqs",
    "max_model_len",
    "gpu_memory_utilization",
}

_VIPE_TYPED_FIELDS = {
    "vipe-storage-tos",
}


def _engine_args_extras(
    dumped: Dict[str, Any],
    typed_fields: set[str] = _VLLM_TYPED_FIELDS,
) -> Dict[str, Any]:
    """Return engine_args fields that aren't already mapped.

    These are user-supplied engine-specific flags passed through via
    EngineArgsSpec's lenient model_config (extra='allow').
    """
    return {k: v for k, v in dumped.items() if k not in typed_fields}


def _build_sglang_args(
    spec: ModelDeploymentTemplateSpec,
    ignore_model: bool = False,
    served_model_name: Optional[str] = None,
) -> List[str]:
    """Render SGLang launch_server arguments.

    Keep this intentionally small: only map common template fields whose
    SGLang CLI equivalent is stable, then append raw serve_args so templates
    can carry version-specific flags.
    """
    args: List[str] = []

    if not ignore_model:
        args.extend(["--model-path", spec.model_source.uri])
    if served_model_name and not _pins_served_model_name(spec):
        args.extend(["--served-model-name", served_model_name])
    if spec.model_source.tokenizer_path:
        args.extend(["--tokenizer-path", spec.model_source.tokenizer_path])

    p = spec.parallelism
    if p.tp > 1:
        args.extend(["--tp", str(p.tp)])
    if p.pp > 1:
        args.extend(["--pp", str(p.pp)])
    if p.dp > 1:
        args.extend(["--dp", str(p.dp)])

    ea = spec.engine_args
    if ea.max_model_len is not None:
        args.extend(["--context-length", str(ea.max_model_len)])
    if ea.max_num_seqs is not None:
        args.extend(["--max-running-requests", str(ea.max_num_seqs)])
    if ea.gpu_memory_utilization is not None:
        args.extend(["--mem-fraction-static", str(ea.gpu_memory_utilization)])

    extras = _engine_args_extras(ea.model_dump(exclude_none=True), _SGLANG_TYPED_FIELDS)
    for key, value in sorted(extras.items()):
        flag = "--" + key.strip().lstrip("-").replace("_", "-")
        if isinstance(value, bool):
            if value:
                args.append(flag)
        elif isinstance(value, list):
            for v in value:
                args.extend([flag, str(v)])
        elif value is None or str(value).strip() == "":
            args.append(flag)
        else:
            args.extend([flag, str(value).strip()])

    q = spec.quantization
    if q.weight is not None:
        args.extend(["--quantization", q.weight.value])
    if q.kv_cache is not None:
        args.extend(["--kv-cache-dtype", q.kv_cache.value])

    args.extend(spec.engine.serve_args)
    return args


def _pins_served_model_name(spec: ModelDeploymentTemplateSpec) -> bool:
    """Whether the template already sets --served-model-name explicitly,
    via an engine_args extra or a raw serve_args entry."""
    extras = _engine_args_extras(spec.engine_args.model_dump(exclude_none=True))
    for key in extras:
        if key.strip().lstrip("-").replace("-", "_") == "served_model_name":
            return True
    for arg in spec.engine.serve_args:
        if arg == "--served-model-name" or arg.startswith("--served-model-name="):
            return True
    return False


def _build_mock_args(spec: ModelDeploymentTemplateSpec) -> List[str]:
    """Mock engine args.

    The mock image (aibrix/vllm-mock:nightly) runs a Python app that
    speaks the OpenAI HTTP contract without real inference. Used by
    integration tests where launching a real engine is wasteful.

    Falls back to admin-supplied serve_args when present, otherwise
    the legacy default that current k8s_job_template.yaml uses.
    """
    if spec.engine.serve_args:
        return list(spec.engine.serve_args)
    # Match the existing k8s_job_template.yaml mock invocation so the
    # renderer is byte-equivalent for legacy mock templates.
    return ["WORKER_VICTIM=1 python app.py || true"]


def needs_shell_wrapper(engine: EngineSpec) -> bool:
    """Whether the engine's args list should be wrapped with /bin/sh -c.

    The mock engine concatenates a shell-style command; vLLM gets its
    args directly to the entrypoint.
    """
    return engine.type == EngineType.MOCK


def _build_vipe_args(
    spec: ModelDeploymentTemplateSpec,
    ignore_model: bool = False,
    served_model_name: Optional[str] = None,
) -> List[str]:
    """Render ViPE launch_server arguments."""
    args: List[str] = []

    if served_model_name and not _pins_served_model_name(spec):
        args.extend(["--vipe-default-model", served_model_name])

    ea = spec.engine_args
    extras = _engine_args_extras(ea.model_dump(exclude_none=True), _VIPE_TYPED_FIELDS)
    for key, value in sorted(extras.items()):
        flag = "--" + key.strip().lstrip("-").replace("_", "-")
        if isinstance(value, bool):
            if value:
                args.append(flag)
        elif isinstance(value, list):
            for v in value:
                args.extend([flag, str(v)])
        elif value is None or str(value).strip() == "":
            args.append(flag)
        else:
            args.extend([flag, str(value).strip()])

    args.extend(spec.engine.serve_args)
    return args
