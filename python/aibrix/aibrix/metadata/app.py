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
import argparse
import json
import os
import sys
from contextlib import asynccontextmanager
from typing import Any, Dict, Optional

import uvicorn
from fastapi import APIRouter, FastAPI, HTTPException, Request, Response
from fastapi.responses import JSONResponse
from kubernetes import client as k8s_client
from kubernetes import config

from aibrix.batch import BatchDriver
from aibrix.batch.client import (
    EndpointSource,
    NoopEndpointSource,
)
from aibrix.batch.state import JobStore
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger, logging_basic_config
from aibrix.metadata.api.v1 import batch, files, models, users
from aibrix.metadata.core import HTTPXClientWrapper
from aibrix.metadata.setting import settings
from aibrix.metadata.store import RedisMetadataStore
from aibrix.storage import create_storage
from aibrix.storage.redis_upgrade import maybe_upgrade_storage

logger = init_logger(__name__)
router = APIRouter()

_MAX_LOGGED_BODY_BYTES = 8192
_LOG_HTTP_BODIES = os.getenv("AIBRIX_MDS_HTTP_BODY_LOG", "").lower() in (
    "1",
    "true",
    "yes",
)


def _load_batch_k8s_context(
    args, context: Optional[InfrastructureContext] = None
) -> InfrastructureContext:
    if context is None:
        context = InfrastructureContext()

    if args.dry_run:
        return context

    if args.enable_k8s_support:
        context.core_v1_api = k8s_client.CoreV1Api()
        context.apps_v1_api = k8s_client.AppsV1Api()

    return context


def _pretty_body(b: bytes) -> str:
    """Indent JSON bodies for readable traffic dumps; truncate oversized."""
    if not b:
        return "(empty)"
    try:
        out = json.dumps(json.loads(b), indent=2, ensure_ascii=False)
    except Exception:  # noqa: BLE001
        out = b.decode("utf-8", errors="replace")
    if len(out) > _MAX_LOGGED_BODY_BYTES:
        return out[:_MAX_LOGGED_BODY_BYTES] + "\n...(truncated)"
    return out


def _emit_traffic(
    method: str,
    path: str,
    req_body: bytes,
    status: int,
    resp_body: Optional[bytes],
    resp_ct: str = "",
) -> None:
    """Print one HTTP exchange to stderr in human-readable form.

    Multi-line by design — bypasses structlog so the JSON bodies render with
    indentation. Off the structured-log path so production filters can ignore.
    """
    if not _LOG_HTTP_BODIES:
        print(f"[MDS HTTP] {method} {path} -> {status}", file=sys.stderr, flush=True)
        return
    parts = [f"\n[MDS HTTP] {method} {path} -> {status}"]
    if req_body:
        parts.append("--- request ---")
        parts.append(_pretty_body(req_body))
    if resp_body is not None:
        parts.append("--- response ---")
        parts.append(_pretty_body(resp_body))
    elif resp_ct:
        parts.append(f"(response body skipped: content-type={resp_ct})")
    print("\n".join(parts), file=sys.stderr, flush=True)


@router.get("/healthz")
async def liveness_check():
    # Simply return a 200 status for liveness check
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.get("/readyz")
async def readiness_check(request: Request):
    # Check if metadata store is ready
    try:
        if getattr(request.app.state, "storage_upgrade_in_progress", False):
            return JSONResponse(
                content={
                    "status": "not ready",
                    "error": "storage upgrade in progress",
                },
                status_code=503,
            )
        if hasattr(request.app.state, "metadata_store"):
            ping_ok = await request.app.state.metadata_store.ping()
            if not ping_ok:
                logger.error("Metadata store ping returned a falsy result.")
                return JSONResponse(
                    content={
                        "status": "not ready",
                        "error": "metadata store unavailable",
                    },
                    status_code=503,
                )
        # Backward compatibility: check redis_client if metadata_store not set
        elif hasattr(request.app.state, "redis_client"):
            await request.app.state.redis_client.ping()
        return JSONResponse(content={"status": "ready"}, status_code=200)
    except Exception as e:
        logger.error(f"Metadata store health check failed: {e}")
        return JSONResponse(
            content={"status": "not ready", "error": str(e)}, status_code=503
        )


@router.get("/status")
async def status_check(request: Request):
    """Get detailed status of all components."""
    status: Dict[str, Any] = {
        "httpx_client": {
            "available": hasattr(request.app.state, "httpx_client_wrapper"),
            "status": "initialized"
            if hasattr(request.app.state, "httpx_client_wrapper")
            else "not_initialized",
        },
        "batch_driver": {
            "available": hasattr(request.app.state, "batch_driver"),
        },
    }

    return JSONResponse(content=status, status_code=200)


@asynccontextmanager
async def lifespan(app: FastAPI):
    # Code executed on startup
    logger.info("Initializing FastAPI app...")
    app.state.storage_upgrade_in_progress = True

    # Initialize metadata store (abstraction over Redis) only if not already set
    # (e.g., tests may pre-configure a mock store before lifespan runs)
    if not hasattr(app.state, "metadata_store") or app.state.metadata_store is None:
        metadata_store = RedisMetadataStore()
        app.state.metadata_store = metadata_store
        # Backward compatibility: expose underlying Redis client for components
        # that haven't migrated to the MetadataStore interface yet
        app.state.redis_client = metadata_store.client
        logger.info("Metadata store initialized")

    try:
        for storage in _iter_upgrade_candidate_storages(app):
            await maybe_upgrade_storage(storage)
    finally:
        app.state.storage_upgrade_in_progress = False

    if hasattr(app.state, "httpx_client_wrapper"):
        app.state.httpx_client_wrapper.start()
    if hasattr(app.state, "batch_driver"):
        await app.state.batch_driver.start()
    yield

    # Code executed on shutdown
    logger.info("Finalizing FastAPI app...")
    if hasattr(app.state, "batch_driver"):
        await app.state.batch_driver.stop()
    if hasattr(app.state, "httpx_client_wrapper"):
        await app.state.httpx_client_wrapper.stop()
    if hasattr(app.state, "metadata_store"):
        await app.state.metadata_store.close()
    elif hasattr(app.state, "redis_client"):
        await app.state.redis_client.aclose()  # type: ignore[attr-defined]
        logger.info("Redis client closed")


def _iter_upgrade_candidate_storages(app: FastAPI):
    seen_ids: set[int] = set()

    def _yield_once(storage):
        if storage is None:
            return
        storage_id = id(storage)
        if storage_id in seen_ids:
            return
        seen_ids.add(storage_id)
        yield storage

    if hasattr(app.state, "storage"):
        yield from _yield_once(app.state.storage)

    try:
        from aibrix.batch.storage import batch_metastore, batch_storage

        if batch_storage.p_storage is not None:
            yield from _yield_once(batch_storage.p_storage.storage)
        yield from _yield_once(batch_metastore.p_metastore)
    except Exception:
        logger.exception("Failed to inspect batch storages for upgrade")  # type: ignore[call-arg]


def build_app(args: argparse.Namespace, params={}):
    if args.enable_fastapi_docs:
        app = FastAPI(lifespan=lifespan, debug=False, redirect_slashes=False)
    else:
        app = FastAPI(
            lifespan=lifespan,
            debug=False,
            openapi_url=None,
            docs_url=None,
            redoc_url=None,
            redirect_slashes=False,
        )

    if args.enable_k8s_support:
        try:
            config.load_incluster_config()
        except Exception:
            # Local debug
            config.load_kube_config()

    app.state.httpx_client_wrapper = HTTPXClientWrapper()

    # Normalize HTTPException responses to OpenAI's top-level
    # ``{"error": {message, type, param, code}}`` shape so that the
    # official ``openai`` SDK can deserialize 4xx responses. FastAPI's
    # default wraps the raw detail under ``{"detail": ...}``, which
    # the SDK cannot read.
    @app.exception_handler(HTTPException)
    async def _openai_compat_http_exception_handler(
        request: Request, exc: HTTPException
    ) -> JSONResponse:
        detail = exc.detail
        if isinstance(detail, dict) and "error" in detail:
            # Routes that already produced an OpenAI-shaped error body
            # (e.g. files routes via _create_error_response) flow through
            # unchanged.
            body: Dict[str, Any] = detail
        else:
            body = {
                "error": {
                    "message": "" if detail is None else str(detail),
                    "type": "invalid_request_error",
                    "param": None,
                    "code": None,
                }
            }
        return JSONResponse(status_code=exc.status_code, content=body)

    # HTTP traffic dump for debugging — always on. JSON bodies are pretty-
    # printed; multipart uploads and file/stream responses skip body capture
    # to avoid buffering large payloads.
    @app.middleware("http")
    async def _log_http_traffic(request: Request, call_next):
        method = request.method
        path = request.url.path

        if not _LOG_HTTP_BODIES:
            response = await call_next(request)
            print(
                f"[MDS HTTP] {method} {path} -> {response.status_code}",
                file=sys.stderr,
                flush=True,
            )
            return response

        req_ct = request.headers.get("content-type", "")
        req_body: bytes = b""
        if req_ct.startswith("application/json"):
            try:
                req_body = await request.body()
            except Exception:  # noqa: BLE001
                pass

        response = await call_next(request)

        resp_ct = response.headers.get("content-type", "")
        if resp_ct.startswith("application/json"):
            chunks = []
            async for chunk in response.body_iterator:
                chunks.append(chunk)
            resp_body = b"".join(chunks)
            _emit_traffic(method, path, req_body, response.status_code, resp_body)
            return Response(
                content=resp_body,
                status_code=response.status_code,
                headers=dict(response.headers),
                media_type=response.media_type,
            )
        _emit_traffic(
            method, path, req_body, response.status_code, None, resp_ct=resp_ct
        )
        return response

    app.include_router(router)

    # Initialize models API
    app.include_router(
        models.router, prefix=f"{settings.API_V1_STR}/models", tags=["models"]
    )
    logger.info("Models API mounted at /v1/models")

    # Initialize user CRUD API
    app.include_router(users.router, tags=["users"])
    logger.info("User CRUD API mounted")

    # The batch BatchDriver does not get a global inference endpoint: jobs
    # carry their own ``aibrix.runtime.target`` and the per-job runtime
    # (k8s job / deployment) builds its own EndpointSource. The only app-level
    # source is the echo client used by --dry-run.
    endpoint_source: Optional[EndpointSource] = None
    dry_run = getattr(args, "dry_run", False)
    if dry_run:
        endpoint_source = NoopEndpointSource()
        logger.warning(
            "DRY RUN MODE — outputs are echoed inputs, not real model "
            "completions. Refuses to write to non-local storage."
        )

    # Initialize batches API
    if not args.disable_batch_api:
        # Registries are now moved to infrastructure_context for sharing between components
        # The construction of context should before any k8s dependent components'
        # (e.g., k8s job execution) initialization.
        infrastructure_context = _load_batch_k8s_context(args)
        app.state.infrastructure_context = infrastructure_context
        app.state.template_registry = infrastructure_context.template_registry
        app.state.profile_registry = infrastructure_context.profile_registry

        if not args.dry_run:
            if infrastructure_context is None:
                raise RuntimeError("Kubernetes batch context is required")

        # The single entity manager is the metastore-backed JobStore; the
        # substrate (LOCAL / Redis / S3 / TOS) is selected via METASTORE_TYPE, so
        # one store serves every backend. endpoint_source is None outside
        # --dry-run: jobs carry their own aibrix.runtime.target and the per-job
        # runtime builds its own EndpointSource.
        app.state.batch_driver = BatchDriver(
            context=infrastructure_context,
            job_entity_manager=JobStore(),
            storage_type=settings.STORAGE_TYPE,
            metastore_type=settings.METASTORE_TYPE,
            endpoint_source=endpoint_source,
            params=params,
        )
        app.include_router(
            batch.router, prefix=f"{settings.API_V1_STR}/batches", tags=["batches"]
        )  # mount batch api at /v1/batches
        args.disable_file_api = False

    # Initialize fiels API
    if not args.disable_file_api:
        app.state.storage = create_storage(settings.STORAGE_TYPE, **params)
        app.include_router(
            files.router, prefix=f"{settings.API_V1_STR}/files", tags=["files"]
        )  # mount files api at /v1/files

    return app


def nullable_str(val: str):
    if not val or val == "None":
        return None
    return val


def main():
    parser = argparse.ArgumentParser(description=f"Run {settings.PROJECT_NAME}")
    parser.add_argument("--host", type=nullable_str, default=None, help="host name")
    parser.add_argument("--port", type=int, default=8090, help="port number")
    parser.add_argument(
        "--enable-fastapi-docs",
        action="store_true",
        default=False,
        help="Enable FastAPI's OpenAPI schema, Swagger UI, and ReDoc endpoint",
    )
    parser.add_argument(
        "--enable-k8s-support",
        action="store_true",
        default=False,
        help=(
            "Enable Kubernetes support so the batch API can create CoreV1/AppsV1 "
            "clients and run jobs as k8s Jobs/Deployments. Disabled by default."
        ),
    )
    parser.add_argument(
        "--disable-batch-api",
        action="store_true",
        default=False,
        help=(
            "Disable the batch API. Useful for metadata-service deployments "
            "that only serve models/users/health endpoints and should not "
            "depend on batch, Kubernetes, or inference backend setup."
        ),
    )
    parser.add_argument(
        "--disable-file-api",
        action="store_true",
        default=False,
        help=(
            "Disable the files API. Only valid when the batch API is also "
            "disabled, because batch jobs use files as their input/output channel."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        default=False,
        help=(
            "Bundle for dev/CI: forces local storage and metastore, uses an "
            "echo inference client (responses are the request body verbatim, "
            "NOT real model completions). Not crash-safe: in-process multipart "
            "upload ids are kept in memory only, so if the server is killed "
            "mid-batch the partial output is unrecoverable. Use a real K8s "
            "deployment with a redis metastore for crash-safe long-running batches."
        ),
    )
    args = parser.parse_args()

    if args.disable_file_api and not args.disable_batch_api:
        # The batch API needs the files API as its input/output channel.
        parser.error(
            "--disable-file-api requires --disable-batch-api: the batch "
            "API uses the files API as its input/output channel."
        )

    # Bundle: dry-run forces local storage so a stray AWS_* / TOS_*
    # in the environment doesn't accidentally write to a real bucket.
    if args.dry_run:
        from aibrix.storage import StorageType  # local import: avoid cycle

        settings.STORAGE_TYPE = StorageType.LOCAL
        settings.METASTORE_TYPE = StorageType.LOCAL

    global logger
    logging_basic_config(settings)
    logger = init_logger(__name__)  # Reset logger

    logger.info(f"Using {args} to startup app", project=settings.PROJECT_NAME)  # type: ignore[call-arg]
    app = build_app(args=args)
    uvicorn.run(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()
