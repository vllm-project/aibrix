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
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, Dict, Optional

import redis.asyncio as redis
import uvicorn
from fastapi import APIRouter, FastAPI, Request
from fastapi.responses import JSONResponse
from kubernetes import config

from aibrix import envs
from aibrix.batch import BatchDriver
from aibrix.batch.job_entity import JobEntityManager
from aibrix.logger import init_logger, logging_basic_config
from aibrix.metadata.api.v1 import batch, files, models, users
from aibrix.metadata.cache import JobCache
from aibrix.metadata.core import HTTPXClientWrapper, KopfOperatorWrapper
from aibrix.metadata.setting import settings
from aibrix.storage import create_storage

logger = init_logger(__name__)
router = APIRouter()


@router.get("/healthz")
async def liveness_check():
    # Simply return a 200 status for liveness check
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.get("/readyz")
async def readiness_check(request: Request):
    # Check if Redis is ready
    try:
        if hasattr(request.app.state, "redis_client"):
            await request.app.state.redis_client.ping()
        return JSONResponse(content={"status": "ready"}, status_code=200)
    except Exception as e:
        logger.error(f"Redis health check failed: {e}")
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
        "kopf_operator": {
            "available": hasattr(request.app.state, "kopf_operator_wrapper"),
        },
        "batch_driver": {
            "available": hasattr(request.app.state, "batch_driver"),
        },
    }

    # Get detailed kopf operator status if available
    if hasattr(request.app.state, "kopf_operator_wrapper"):
        kopf_status = request.app.state.kopf_operator_wrapper.get_status()
        status["kopf_operator"].update(kopf_status)

    return JSONResponse(content=status, status_code=200)


@asynccontextmanager
async def lifespan(app: FastAPI):
    # Code executed on startup
    logger.info("Initializing FastAPI app...")

    # Initialize Redis client
    app.state.redis_client = redis.Redis(
        host=envs.STORAGE_REDIS_HOST or "localhost",
        port=envs.STORAGE_REDIS_PORT,
        db=envs.STORAGE_REDIS_DB,
        password=envs.STORAGE_REDIS_PASSWORD,
        decode_responses=False,
    )
    logger.info(
        f"Redis client initialized: {envs.STORAGE_REDIS_HOST}:{envs.STORAGE_REDIS_PORT}"
    )

    if hasattr(app.state, "httpx_client_wrapper"):
        app.state.httpx_client_wrapper.start()
    if hasattr(app.state, "kopf_operator_wrapper"):
        app.state.kopf_operator_wrapper.start()
    if hasattr(app.state, "batch_driver"):
        await app.state.batch_driver.start()
    yield

    # Code executed on shutdown
    logger.info("Finalizing FastAPI app...")
    if hasattr(app.state, "batch_driver"):
        await app.state.batch_driver.stop()
    if hasattr(app.state, "kopf_operator_wrapper"):
        app.state.kopf_operator_wrapper.stop()
    if hasattr(app.state, "httpx_client_wrapper"):
        await app.state.httpx_client_wrapper.stop()
    if hasattr(app.state, "redis_client"):
        await app.state.redis_client.aclose()  # type: ignore[attr-defined]
        logger.info("Redis client closed")


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

    app.state.httpx_client_wrapper = HTTPXClientWrapper()

    # Initialize kopf operator wrapper if K8s jobs are enabled
    if args.enable_k8s_job:
        app.state.kopf_operator_wrapper = KopfOperatorWrapper(
            namespace=getattr(args, "k8s_namespace", "default"),
            startup_timeout=getattr(args, "kopf_startup_timeout", 30.0),
            shutdown_timeout=getattr(args, "kopf_shutdown_timeout", 10.0),
        )

    app.include_router(router)

    # Initialize models API
    app.include_router(
        models.router, prefix=f"{settings.API_V1_STR}/models", tags=["models"]
    )
    logger.info("Models API mounted at /v1/models")

    # Initialize user CRUD API
    app.include_router(users.router, tags=["users"])
    logger.info("User CRUD API mounted")

    # Initialize batches API
    if not args.disable_batch_api:
        job_entity_manager: Optional[JobEntityManager] = None
        if args.enable_k8s_job:
            # Get template_path from params if provided
            job_entity_manager = JobCache(template_patch_path=args.k8s_job_patch)
        app.state.batch_driver = BatchDriver(
            job_entity_manager,
            storage_type=settings.STORAGE_TYPE,
            metastore_type=settings.METASTORE_TYPE,
            stand_alone=True,
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
    parser.add_argument("--port", type=int, default=8100, help="port number")
    parser.add_argument(
        "--enable-fastapi-docs",
        action="store_true",
        default=False,
        help="Enable FastAPI's OpenAPI schema, Swagger UI, and ReDoc endpoint",
    )
    parser.add_argument(
        "--disable-batch-api",
        action="store_true",
        default=False,
        help="Disable batch api",
    )
    parser.add_argument(
        "--disable-file-api",
        action="store_true",
        default=False,
        help="Disable file api",
    )
    parser.add_argument(
        "--enable-k8s-job",
        action="store_true",
        default=False,
        help="Enable native kubernetes jobs as the job executor",
    )
    parser.add_argument(
        "--k8s-namespace",
        type=str,
        default="default",
        help="Kubernetes namespace to monitor for jobs (default: default)",
    )
    parser.add_argument(
        "--k8s-job-patch",
        type=Path,
        default=None,
        help="Patch to customize k8s job template",
    )
    parser.add_argument(
        "--kopf-startup-timeout",
        type=float,
        default=30.0,
        help="Timeout in seconds for kopf operator startup (default: 30.0)",
    )
    parser.add_argument(
        "--kopf-shutdown-timeout",
        type=float,
        default=10.0,
        help="Timeout in seconds for kopf operator shutdown (default: 10.0)",
    )
    parser.add_argument(
        "--e2e-test",
        action="store_true",
        default=False,
        help="Enable features for e2e test",
    )
    args = parser.parse_args()

    global logger
    logging_basic_config(settings)
    logger = init_logger(__name__)  # Reset logger

    try:
        config.load_incluster_config()
    except Exception:
        # Local debug
        config.load_kube_config()

    logger.info(f"Using {args} to startup app", project=settings.PROJECT_NAME)  # type: ignore[call-arg]
    app = build_app(args=args)
    uvicorn.run(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()
