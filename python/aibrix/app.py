import os
import shutil
from pathlib import Path

import uvicorn
from fastapi import FastAPI
from prometheus_client import CollectorRegistry, make_asgi_app, multiprocess
from starlette.routing import Mount

from aibrix import envs
from aibrix.logger import init_logger
from aibrix.metrics.engine_rules import get_metric_standard_rules
from aibrix.metrics.http_collector import HTTPCollector

logger = init_logger(__name__)


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


def mount_metrics(app: FastAPI):
    # setup multiprocess collector
    initial_prometheus_multiproc_dir()
    prometheus_multiproc_dir_path = os.environ["PROMETHEUS_MULTIPROC_DIR"]
    logger.info(
        f"AIBrix to use {prometheus_multiproc_dir_path} as PROMETHEUS_MULTIPROC_DIR"
    )
    registry = CollectorRegistry()
    multiprocess.MultiProcessCollector(registry)

    # construct scrape metric config
    engine = envs.METRIC_SCRAPE_ENGINE
    scrape_host = envs.METRIC_SCRAPE_HOST
    scrape_port = envs.METRIC_SCRAPE_PORT
    scrape_path = envs.METRIC_SCRAPE_PATH
    scrape_endpoint = f"http://{scrape_host}:{scrape_port}{scrape_path}"
    collector = HTTPCollector(scrape_endpoint, get_metric_standard_rules(engine))
    registry.register(collector)
    logger.info(
        f"AIBrix to scrape metrics from {scrape_endpoint}, use {engine} standard rules"
    )

    # Add prometheus asgi middleware to route /metrics requests
    metrics_route = Mount("/metrics", make_asgi_app(registry=registry))

    app.routes.append(metrics_route)


def build_app():
    app = FastAPI(debug=False)
    mount_metrics(app)
    return app


app = build_app()
uvicorn.run(app, port=envs.SERVER_PORT)
