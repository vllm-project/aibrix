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
import re
import shlex
import signal
import sys
from typing import Callable, Dict, Optional, Sequence, Tuple

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.utils.utils import (
    HTTP_DEFAULT_MAX_INPUT_SIZE,
    validate_positive_double,
    validate_positive_int,
)

EngineBuilder = Callable[[argparse.Namespace], LLMEngine]

# Names set on --setenv are upper-cased with hyphens -> underscores, matching
# the convention already used by --vipe-storage-tos.
_ENV_NAME_RE = re.compile(r"[A-Z_][A-Z0-9_]*")


class SetenvError(ValueError):
    """Raised for a malformed ``--setenv KEY VALUE`` argument."""


def _apply_setenv_args(args: argparse.Namespace) -> None:
    """Apply ``--setenv KEY VALUE`` pairs to the process environment.

    Keys are upper-cased with ``-`` -> ``_`` (``--setenv tos-endpoint X`` sets
    ``TOS_ENDPOINT``). The shared parsing path applies these variables before
    any engine is built; subprocess engines inherit the same environment.
    """
    items = getattr(args, "setenv", None)
    if not items:
        return
    # Validate each occurrence before applying any environment changes.
    updates: Dict[str, str] = {}
    for group in items:
        if len(group) % 2 != 0:
            raise SetenvError(
                f"--setenv expects KEY VALUE pairs, got {len(group)} items"
            )
        for i in range(0, len(group), 2):
            raw_key, value = group[i], group[i + 1]
            name = raw_key.upper().replace("-", "_")
            if not _ENV_NAME_RE.fullmatch(name):
                raise SetenvError(f"invalid environment variable name {raw_key!r}")
            updates[name] = value
    os.environ.update(updates)


def _reset_inherited_signal_state() -> None:
    """Undo a SIGTERM/SIGINT mask or SIG_IGN inherited from the parent.

    Both survive exec(): a supervisor or wrapper script that blocked or
    ignored these signals would otherwise leave the server -- and uvicorn's
    graceful shutdown -- deaf to ``kill`` / pod termination.
    """
    handled = {signal.SIGINT, signal.SIGTERM}
    if hasattr(signal, "pthread_sigmask"):
        signal.pthread_sigmask(signal.SIG_UNBLOCK, handled)
    for sig in handled:
        if signal.getsignal(sig) == signal.SIG_IGN:
            signal.signal(sig, signal.SIG_DFL)


def add_common_args(parser: argparse.ArgumentParser):
    parser.add_argument(
        "--setenv",
        type=str,
        nargs="+",
        action="append",
        metavar="KEY VALUE",
        help="Set environment variables (KEY upper-cased, '-' -> '_') before "
        "any engine is built, e.g. --setenv tos-endpoint "
        "https://tos.example.com sets TOS_ENDPOINT. Repeated flags and "
        "KEY VALUE pairs in one flag are both supported.",
    )
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
    # explicitly set STORAGE_TYPE to tos
    os.environ["STORAGE_TYPE"] = "tos"
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


def add_proxy_args(parser: argparse.ArgumentParser):
    proxy_group = parser.add_argument_group("Proxy engine arguments")
    proxy_group.add_argument(
        "--proxy-engine-cmd",
        type=str,
        nargs="+",
        required=True,
        help="Command to launch the inner engine as a subprocess. Quote the whole "
        "command when it contains options, e.g. --proxy-engine-cmd "
        "'python -m vllm.entrypoints.openai.api_server'. Parsed as argv, not a shell.",
    )
    proxy_group.add_argument(
        "--proxy-engine-host",
        type=str,
        default="127.0.0.1",
        help="Host for the inner engine HTTP server (default: 127.0.0.1)",
    )
    proxy_group.add_argument(
        "--proxy-engine-port",
        type=int,
        default=8001,
        help="Port for the inner engine HTTP server (default: 8001)",
    )
    proxy_group.add_argument(
        "--proxy-engine-spec",
        type=str,
        default="vllm",
        help="Engine spec for endpoint mapping (default: vllm). "
        "Determines which OpenAI APIs the inner engine supports "
        "and their endpoint paths.",
    )
    proxy_group.add_argument(
        "--proxy-startup-timeout",
        type=validate_positive_double,
        default=1800.0,
        help="Seconds to wait for the inner engine to become healthy (default: 1800).",
    )
    proxy_group.add_argument(
        "--proxy-health-check-interval",
        type=validate_positive_double,
        default=10.0,
        help="Seconds between inner-engine liveness checks. Determines how "
        "quickly a crashed engine is detected and the proxy marked failed "
        "(default: 10).",
    )
    proxy_group.add_argument(
        "--proxy-shutdown-grace",
        type=validate_positive_double,
        default=30.0,
        help="Seconds between SIGTERM and SIGKILL when stopping the inner "
        "engine process tree (default: 30).",
    )


def build_proxy_engine(args: argparse.Namespace) -> LLMEngine:
    from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
    from aibrix.openai_frontend.proxy.engine_spec import get_engine_spec

    spec = get_engine_spec(args.proxy_engine_spec)
    return ProxyEngine(
        engine_cmd=args.proxy_engine_cmd,
        engine_port=args.proxy_engine_port,
        engine_host=args.proxy_engine_host,
        spec=spec,
        health_check_interval=getattr(args, "proxy_health_check_interval", 10.0),
        startup_timeout=getattr(args, "proxy_startup_timeout", 1800.0),
        shutdown_grace_seconds=getattr(args, "proxy_shutdown_grace", 30.0),
    )


ENGINE_REGISTRY: Dict[
    str, Tuple[EngineBuilder, Callable[[argparse.ArgumentParser], None]]
] = {
    "vipe": (build_vipe_engine, add_vipe_args),
    "proxy": (build_proxy_engine, add_proxy_args),
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
    args = parser.parse_args(argv)
    if selected_engine == "proxy" and len(args.proxy_engine_cmd) == 1:
        try:
            args.proxy_engine_cmd = shlex.split(args.proxy_engine_cmd[0])
        except ValueError as e:
            parser.error(f"Invalid --proxy-engine-cmd: {e}")
        if not args.proxy_engine_cmd:
            parser.error("--proxy-engine-cmd must not be empty")
    try:
        _apply_setenv_args(args)
    except SetenvError as e:
        parser.error(str(e))
    return args


def build_engine(args: argparse.Namespace) -> LLMEngine:
    try:
        return ENGINE_REGISTRY[args.engine][0](args)
    except Exception as e:
        raise ValueError(f"Failed to initialize engine '{args.engine}': {e}") from e


def main():
    try:
        args = parse_args()
    except SystemExit:
        raise
    log_level = getattr(logging, args.uvicorn_log_level.upper(), logging.INFO)
    logging.basicConfig(
        level=log_level,
        format="%(asctime)s %(levelname)s %(filename)s:%(lineno)d %(message)s",
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

    # Uvicorn owns SIGINT/SIGTERM and drives the engine's async lifespan cleanup.
    _reset_inherited_signal_state()
    try:
        openai_frontend.start()
    except KeyboardInterrupt:
        # Uvicorn re-raises SIGINT after lifespan cleanup has completed.
        pass


if __name__ == "__main__":
    main()
