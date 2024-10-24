import os
import shutil
from pathlib import Path
import time
from urllib.parse import urljoin

from aibrix.config import EXCLUDE_METRICS_HTTP_ENDPOINTS
from aibrix.metrics.metrics import (
    HTTP_COUNTER_METRICS,
    HTTP_LATENCY_METRICS,
    INFO_METRICS,
    REGISTRY,
)
import uvicorn
from fastapi import APIRouter, FastAPI, Request, Response
from fastapi.datastructures import State
from fastapi.responses import JSONResponse
from prometheus_client import make_asgi_app, multiprocess
from starlette.routing import Mount

from aibrix import __version__, envs
from aibrix.logger import init_logger
from aibrix.metrics.engine_rules import get_metric_standard_rules
from aibrix.metrics.http_collector import HTTPCollector
from aibrix.openapi.engine.base import InferenceEngine, get_inference_engine
from aibrix.openapi.protocol import (
    ErrorResponse,
    LoadLoraAdapterRequest,
    UnloadLoraAdapterRequest,
)

logger = init_logger(__name__)
router = APIRouter()


def initial_prometheus_multiproc_dir():
    if "PROMETHEUS_MULTIPROC_DIR" not in os.environ:
        prometheus_multiproc_dir = envs.PROMETHEUS_MULTIPROC_DIR
    else:
        prometheus_multiproc_dir = os.environ["PROMETHEUS_MULTIPROC_DIR"]

    # Note: ensure it will be automatically cleaned up upon exit.
    path = Path(prometheus_multiproc_dir)
    path.mkdir(parents=True, exist_ok=True)
    if path.is_dir():
        for item in path.iterdir():
            if item.is_dir():
                shutil.rmtree(item)
            else:
                item.unlink()
    os.environ["PROMETHEUS_MULTIPROC_DIR"] = envs.PROMETHEUS_MULTIPROC_DIR


def inference_engine(request: Request) -> InferenceEngine:
    return request.app.state.inference_engine


def mount_metrics(app: FastAPI):
    # setup multiprocess collector
    initial_prometheus_multiproc_dir()
    prometheus_multiproc_dir_path = os.environ["PROMETHEUS_MULTIPROC_DIR"]
    logger.info(
        f"AIBrix to use {prometheus_multiproc_dir_path} as PROMETHEUS_MULTIPROC_DIR"
    )
    # registry = CollectorRegistry()
    multiprocess.MultiProcessCollector(REGISTRY)

    # construct scrape metric config
    engine = envs.INFERENCE_ENGINE

    scrape_endpoint = urljoin(envs.INFERENCE_ENGINE_ENDPOINT, envs.METRIC_SCRAPE_PATH)
    collector = HTTPCollector(scrape_endpoint, get_metric_standard_rules(engine))
    REGISTRY.register(collector)
    logger.info(
        f"AIBrix to scrape metrics from {scrape_endpoint}, use {engine} standard rules"
    )

    # Add prometheus asgi middleware to route /metrics requests
    metrics_route = Mount("/metrics", make_asgi_app(registry=REGISTRY))

    app.routes.append(metrics_route)


def init_app_state(state: State) -> None:
    state.inference_engine = get_inference_engine(
        envs.INFERENCE_ENGINE,
        envs.INFERENCE_ENGINE_VERSION,
        envs.INFERENCE_ENGINE_ENDPOINT,
    )


@router.post("/v1/lora_adapter/load")
async def load_lora_adapter(request: LoadLoraAdapterRequest, raw_request: Request):
    response = await inference_engine(raw_request).load_lora_adapter(request)
    if isinstance(response, ErrorResponse):
        return JSONResponse(content=response.model_dump(), status_code=response.code)

    return Response(status_code=200, content=response)


@router.post("/v1/lora_adapter/unload")
async def unload_lora_adapter(request: UnloadLoraAdapterRequest, raw_request: Request):
    response = await inference_engine(raw_request).unload_lora_adapter(request)
    if isinstance(response, ErrorResponse):
        return JSONResponse(content=response.model_dump(), status_code=response.code)

    return Response(status_code=200, content=response)


def build_app():
    app = FastAPI(debug=False)
    INFO_METRICS.info(
        {
            "version": __version__.__version__,
            "engine": envs.INFERENCE_ENGINE,
            "engine_version": envs.INFERENCE_ENGINE_VERSION,
        }
    )
    mount_metrics(app)
    init_app_state(app.state)
    app.include_router(router)
    return app


app = build_app()


@app.middleware("http")
async def add_router_prometheus_middlerware(request: Request, call_next):
    method = request.method
    endpoint = request.scope.get("path")
    # Exclude endpoints that do not require metrics
    if endpoint in EXCLUDE_METRICS_HTTP_ENDPOINTS:
        response = await call_next(request)
        return response

    start_time = time.perf_counter()
    response = await call_next(request)
    process_time = time.perf_counter() - start_time

    status = response.status_code
    HTTP_LATENCY_METRICS.labels(
        method=method, endpoint=endpoint, status=status
    ).observe(process_time)
    HTTP_COUNTER_METRICS.labels(method=method, endpoint=endpoint, status=status).inc()
    return response


if __name__ == "__main__":
    uvicorn.run(app, port=envs.SERVER_PORT)
