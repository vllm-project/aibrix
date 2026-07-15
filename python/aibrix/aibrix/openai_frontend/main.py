#!/usr/bin/env python3
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

import argparse
import importlib
import logging
import os
import signal
import sys
from functools import partial
from typing import Callable, Dict, Optional, Sequence, Tuple

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.utils.utils import (
    HTTP_DEFAULT_MAX_INPUT_SIZE,
    validate_positive_double,
    validate_positive_int,
)

EngineBuilder = Callable[[argparse.Namespace], LLMEngine]


def signal_handler(openai_frontend, signal_num, frame):
    print(f"Received signal={signal_num}, frame={frame}")
    shutdown(openai_frontend)


def shutdown(openai_frontend):
    print("Shutting down AIBrix OpenAI-Compatible Frontend...")
    openai_frontend.stop()


def add_common_args(parser: argparse.ArgumentParser):
    parser.add_argument(
        "--host",
        type=str,
        default="0.0.0.0",
        help="Address/host of frontend (default: '0.0.0.0')",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8000,
        help="HTTP port (default: 8000)",
    )
    parser.add_argument(
        "--uvicorn-log-level",
        type=str,
        default="info",
        choices=["debug", "info", "warning", "error", "critical", "trace"],
        help="log level for uvicorn",
    )
    parser.add_argument(
        "--openai-restricted-api",
        type=str,
        default=None,
        nargs=3,
        metavar=("APIs", "Restricted Key", "Restricted Value"),
        action="append",
        help=(
            "Restrict access to specific OpenAI API endpoints. Format: "
            "'<API_1>,<API_2>,... <restricted-key> <restricted-value>'."
        ),
    )
    parser.add_argument(
        "--http-max-input-size",
        type=validate_positive_int,
        default=HTTP_DEFAULT_MAX_INPUT_SIZE,
        help=(
            "Maximum allowed HTTP request input size in bytes for the OpenAI "
            f"frontend (default: {HTTP_DEFAULT_MAX_INPUT_SIZE}, i.e. 64 MiB)."
        ),
    )


def add_vipe_args(parser: argparse.ArgumentParser):
    vipe_group = parser.add_argument_group("ViPE engine arguments")
    vipe_group.add_argument(
        "--vipe-pipeline-type",
        type=str,
        default="default",
        help="Pipeline type for engine=vipe",
    )
    vipe_group.add_argument(
        "--vipe-default-model",
        type=str,
        default="vipe",
        help="Default model id exposed by /v1/models when engine=vipe",
    )
    vipe_group.add_argument(
        "--vipe-storage-tos",
        type=str,
        nargs="+",
        metavar="KEY VALUE",
        help="Override STORAGE_TOS_<KEY> for engine=vipe as key-value pairs, "
        "e.g. --vipe-storage-tos access-key XXX endpoint XXX bucket XXX",
    )
    vipe_group.add_argument(
        "--vipe-max-video-download-bytes",
        type=validate_positive_int,
        default=512 * 1024 * 1024,
        help="Maximum downloaded size for 'video_url' in bytes (default: 536870912, i.e. 512 MiB)",
    )
    vipe_group.add_argument(
        "--vipe-gpu-memory-utilization",
        type=validate_positive_double,
        default=0.9,
        help="GPU memory utilization for GPU (default: 0.9)",
    )
    vipe_group.add_argument(
        "--vipe-max-seq-num",
        type=validate_positive_int,
        default=32,
        help="Maximum sequence number (default: 32)",
    )


def _apply_vipe_storage_env(args: argparse.Namespace):
    items = args.vipe_storage_tos
    if not items or len(items) < 2:
        return
    if len(items) % 2 != 0:
        raise ValueError(
            f"--vipe-storage-tos expects key-value pairs, got {len(items)} items: {items}"
        )
    for i in range(0, len(items), 2):
        key, value = items[i], items[i + 1]
        os.environ[f"STORAGE_TOS_{key.upper().replace('-', '_')}"] = value


def _reload_env_module_cache():
    envs = importlib.import_module("aibrix.envs")
    importlib.reload(envs)


def build_vipe_engine(args: argparse.Namespace) -> LLMEngine:
    _apply_vipe_storage_env(args)
    _reload_env_module_cache()
    from aibrix.openai_frontend.engine.vipe_engine import ViPEEngine

    return ViPEEngine(
        pipeline_type=args.vipe_pipeline_type,
        gpu_memory_utilization=args.vipe_gpu_memory_utilization,
        max_seq_num=args.vipe_max_seq_num,
        default_model=args.vipe_default_model,
        max_video_download_bytes=args.vipe_max_video_download_bytes,
    )


ENGINE_REGISTRY: Dict[
    str, Tuple[EngineBuilder, Callable[[argparse.ArgumentParser], None]]
] = {
    "vipe": (build_vipe_engine, add_vipe_args),
}


def _build_parser(engine: Optional[str] = None) -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="AIBrix OpenAI-Compatible RESTful API server."
    )
    parser.add_argument(
        "--engine",
        type=str,
        default="vipe",
        choices=sorted(ENGINE_REGISTRY.keys()),
        help="Engine backend to use",
    )
    add_common_args(parser)

    if engine is not None:
        ENGINE_REGISTRY[engine][1](parser)

    return parser


def _parse_engine_arg(argv: Optional[Sequence[str]] = None) -> str:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--engine", type=str, default="vipe")
    engine_args, _ = parser.parse_known_args(argv)
    return engine_args.engine


def parse_args(argv: Optional[Sequence[str]] = None):
    selected_engine = _parse_engine_arg(argv)
    if selected_engine not in ENGINE_REGISTRY:
        parser = _build_parser()
        return parser.parse_args(argv)

    parser = _build_parser(selected_engine)
    return parser.parse_args(argv)


def build_engine(args: argparse.Namespace) -> LLMEngine:
    try:
        return ENGINE_REGISTRY[args.engine][0](args)
    except Exception as e:
        raise ValueError(f"Failed to initialize engine '{args.engine}': {e}") from e


def main():
    args = parse_args()
    log_level = getattr(logging, args.uvicorn_log_level.upper(), logging.INFO)
    logging.basicConfig(
        level=log_level, format="%(asctime)s %(levelname)s %(name)s: %(message)s"
    )

    try:
        engine = build_engine(args)
        openai_frontend = FastApiFrontend(
            engine=engine,
            host=args.host,
            port=args.port,
            log_level=args.uvicorn_log_level,
            restricted_apis=args.openai_restricted_api,
            http_max_input_size=args.http_max_input_size,
        )
    except ValueError as e:
        print(
            f"[ERROR] Failed to initialize FastAPI frontend: {e}",
            file=sys.stderr,
        )
        sys.exit(1)

    signal.signal(signal.SIGINT, partial(signal_handler, openai_frontend))
    signal.signal(signal.SIGTERM, partial(signal_handler, openai_frontend))

    openai_frontend.start()


if __name__ == "__main__":
    main()
