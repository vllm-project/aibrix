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
from typing import Optional

import uvicorn
from fastapi import APIRouter, FastAPI
from fastapi.responses import JSONResponse

from aibrix.batch import BatchDriver
from aibrix.batch.job_entity import JobEntityManager
from aibrix.metadata.api.v1 import batch, files
from aibrix.metadata.core.httpx_client import HTTPXClientWrapper
from aibrix.metadata.logger import init_logger
from aibrix.metadata.setting import settings

from .cache import JobCache

logger = init_logger(__name__)
router = APIRouter()


@router.get("/healthz")
async def liveness_check():
    # Simply return a 200 status for liveness check
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.get("/ready")
async def readiness_check():
    # Check if the inference engine is ready
    return JSONResponse(content={"status": "ready"}, status_code=200)


@asynccontextmanager
async def lifespan(app: FastAPI):
    # Code executed on startup
    if hasattr(app.state, "httpx_client_wrapper"):
        app.state.httpx_client_wrapper.start()
    yield

    # Code executed on shutdown
    if hasattr(app.state, "job_controller"):
        await app.state.job_controller.close()
    if hasattr(app.state, "httpx_client_wrapper"):
        await app.state.httpx_client_wrapper.stop()


def build_app(args: argparse.Namespace):
    if args.enable_fastapi_docs:
        app = FastAPI(lifespan=lifespan, debug=False)
    else:
        app = FastAPI(
            lifespan=lifespan,
            debug=False,
            openapi_url=None,
            docs_url=None,
            redoc_url=None,
        )

    app.state.httpx_client_wrapper = HTTPXClientWrapper()
    app.include_router(router)
    if not args.disable_batch_api:
        job_entity_manager: Optional[JobEntityManager] = None
        if args.enable_k8s_job:
            job_entity_manager = JobCache()
        app.state.job_controller = BatchDriver(job_entity_manager)
        app.include_router(
            batch.router, prefix=f"{settings.API_V1_STR}/batches", tags=["batches"]
        )  # mount batch api at /v1/batches
        if args.e2e_test:
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
        "--enable-k8s-job",
        action="store_true",
        default=False,
        help="Enable native kubernetes jobs as the job executor",
    )
    parser.add_argument(
        "--e2e-test",
        action="store_true",
        default=False,
        help="Enable features for e2e test",
    )
    args = parser.parse_args()

    logger.info(f"Using {args} to startup {settings.PROJECT_NAME}")
    app = build_app(args=args)
    uvicorn.run(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()
