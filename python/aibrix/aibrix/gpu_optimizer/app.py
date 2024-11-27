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

import logging
import os
import threading
from typing import Dict, Optional, Tuple

import redis
import uvicorn
from kubernetes import client, config, watch
from starlette.applications import Starlette
from starlette.responses import JSONResponse, PlainTextResponse

from aibrix.gpu_optimizer.load_monitor.load_reader import GatewayLoadReader
from aibrix.gpu_optimizer.load_monitor.monitor import DeploymentStates, ModelMonitor
from aibrix.gpu_optimizer.load_monitor.profile_reader import RedisProfileReader
from aibrix.gpu_optimizer.load_monitor.visualizer import mount_to as mount_visulizer
from aibrix.gpu_optimizer.utils import ExcludePathsFilter

NAMESPACE = os.getenv("NAMESPACE", "aibrix-system")
MODEL_LABEL = "model.aibrix.ai/name"
MIN_REPLICAS_LABEL = "model.aibrix.ai/min_replicas"
REDIS_HOST = os.getenv("REDIS_HOST", "localhost")
REDIS_PORT = int(os.getenv("REDIS_PORT", 6379))

routes = []  # type: ignore
model_monitors: Dict[str, ModelMonitor] = {}  # Dictionary to store serving threads

mount_visulizer(
    routes, "/dash/{model_name}", lambda model_name: model_monitors.get(model_name)
)
app = Starlette(routes=routes)
redis_client = redis.Redis(host=REDIS_HOST, port=REDIS_PORT, db=0)  # Default DB
debug = False


def validate_model(deployment) -> Tuple[str, Optional[ModelMonitor]]:
    """Validate the deployment and return the model monitor if the deployment is valid."""
    labels = deployment.metadata.labels
    if not labels:
        raise Exception(
            f'No labels found for this deployment, please specify "{MODEL_LABEL}" label in deployment.'
        )

    # Access model label
    model_name = labels.get(MODEL_LABEL)
    if not model_name:
        raise Exception(
            f'No "{MODEL_LABEL}" label found for this deployment, please specify "{MODEL_LABEL}" label in deployment.'
        )

    if model_name in model_monitors:
        return model_name, model_monitors[model_name]
    else:
        return model_name, None


def new_deployment(deployment):
    """Return a new DeploymentStates object from the deployment."""
    min_replicas = 0
    labels = deployment.metadata.labels
    if labels:
        label = labels.get(MIN_REPLICAS_LABEL)
        if label:
            min_replicas = int(label)

    return DeploymentStates(
        deployment.metadata.name,
        deployment.spec.replicas if deployment.spec.replicas is not None else 0,
        min_replicas,
    )


def start_serving_thread(watch_ver, deployment, watch_event: bool) -> bool:
    """Start model monitor, returns True if a new server thread is created, False otherwise."""
    model_name, model_monitor = validate_model(deployment)

    # Get deployment specs
    deployment_name = deployment.metadata.name
    namespace = deployment.metadata.namespace

    # Update profile if key exists
    if model_monitor is not None:
        model_monitor.add_deployment(
            watch_ver, deployment_name, namespace, lambda: new_deployment(deployment)
        )
        logger.info(
            f'Deployment "{deployment_name}" added to the model monitor for "{model_name}"'
        )
        return False

    reader = GatewayLoadReader(redis_client, model_name)
    profile = RedisProfileReader(redis_client, model_name)
    model_monitor = ModelMonitor(
        model_name,
        watch_ver,
        reader,
        deployment=new_deployment(deployment),
        namespace=namespace,
        profile_reader=profile,
        debug=debug,
    )
    model_monitor.start()
    model_monitors[model_name] = model_monitor
    if watch_event:
        logger.info(
            f'New model monitor started for "{model_name}". Deployment "{deployment_name}" added.'
        )
    else:
        logger.info(
            f'Model monitor started for existed "{model_name}". Deployment "{deployment_name}" added.'
        )
    return True


def remove_deployment(deployment):
    """Remove deployment from model monitor"""
    model_name, model_monitor = validate_model(deployment)

    deployment_name = deployment.metadata.name
    namespace = deployment.metadata.namespace
    if model_monitor is None:
        logger.warning(
            f'Removing "{deployment_name}" from the model monitor, but "{model_name}" has not monitored.'
        )
        return

    if model_monitor.remove_deployment(deployment_name, namespace) == 0:
        model_monitor.stop()
        del model_monitors[model_name]
        logger.info(
            f'Removing "{deployment_name}" from the model monitor, no deployment left in "{model_name}", stopping the model monitor.'
        )
        return

    logger.info(
        f'Removing "{deployment_name}" from the model monitor for "{model_name}".'
    )


@app.route("/monitor/{namespace}/{deployment_name}", methods=["POST"])
async def start_deployment_optimization(request):
    namespace = request.path_params["namespace"]
    deployment_name = request.path_params["deployment_name"]
    try:
        # Verify the deployment exists
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        # Start the deployment optimization
        if start_serving_thread(None, deployment, True):
            return JSONResponse({"message": "Deployment optimization started"})
        else:
            return JSONResponse({"message": "Deployment optimization already started"})
    except Exception as e:
        return JSONResponse(
            {"error": f"Error starting deployment optimization: {e}"}, status_code=500
        )


@app.route("/monitor/{namespace}/{deployment_name}", methods=["DELETE"])
async def stop_deployment_optimization(request):
    namespace = request.path_params["namespace"]
    deployment_name = request.path_params["deployment_name"]
    try:
        # Verify the deployment exists
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        # Start the deployment optimization
        remove_deployment(deployment)
        return JSONResponse({"message": "Deployment optimization stopped"})
    except Exception as e:
        return JSONResponse(
            {"error": f"Error stopping deployment optimization: {e}"}, status_code=500
        )


@app.route("/scale/{namespace}/{deployment_name}/{replicas}", methods=["POST"])
async def scale_deployment(request):
    namespace = request.path_params["namespace"]
    deployment_name = request.path_params["deployment_name"]
    replicas = request.path_params["replicas"]
    try:
        # Verify the deployment exists
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        _, monitor = validate_model(deployment)
        if monitor is None:
            raise Exception(f'Model "{model_name}" is not monitored.')

        # Set the scaling metrics
        monitor.update_deployment_num_replicas(deployment_name, namespace, replicas)

        return JSONResponse({"message": f"Scaled to {replicas}"})
    except Exception as e:
        return JSONResponse(
            {"error": f"Error starting deployment optimization: {e}"}, status_code=500
        )


@app.route("/metrics/{namespace}/{deployment_name}")
async def get_deployment_metrics(request):
    namespace = request.path_params["namespace"]
    deployment_name = request.path_params["deployment_name"]
    # get deployment information
    try:
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        model_name, monitor = validate_model(deployment)
        if monitor is None:
            raise Exception(f'Model "{model_name}" is not monitored.')

        replicas = monitor.read_deployment_num_replicas(deployment_name, namespace)

        # construct Prometheus-style Metrics
        metrics_output = f"""# HELP vllm:deployment_replicas Number of suggested replicas.
# TYPE vllm:deployment_replicas gauge
vllm:deployment_replicas{{model_name="{model_name}"}} {replicas}
"""
        return PlainTextResponse(metrics_output)
    except Exception as e:
        logger.error(f"Failed to read metrics: {e}")
        return JSONResponse({"error": f"Failed to read metrics: {e}"}, status_code=404)


def main(signal, timeout):
    logger.info(f"Starting GPU optimizer (debug={debug}) ...")
    while not signal["done"]:
        signal["watch"] = None

        # Mark all deployments as outdated
        for model_name, model_monitor in model_monitors.items():
            model_monitor.mark_deployments_outdated()

        try:
            apps_v1 = client.AppsV1Api()

            # List existing deployments
            logger.info(f"Looking for deployments in {NAMESPACE} with {MODEL_LABEL}")
            deployments = apps_v1.list_namespaced_deployment(
                namespace=NAMESPACE, label_selector=MODEL_LABEL
            )
            watch_version = deployments.metadata.resource_version
            logger.debug(f"last watch version: {watch_version}")
            for deployment in deployments.items:
                try:
                    start_serving_thread(watch_version, deployment, False)
                except Exception as e:
                    logger.warning(
                        f"Error on handle existing deployment {deployment.metadata.name}: {e}"
                    )
        except client.rest.ApiException as ae:
            logger.error(
                f"Error connecting to Kubernetes API: {ae}. Please manually initiate GPU optimizer by calling the /monitor/{{namespace}}/{{deployment_name}} endpoint"
            )
            return
        except Exception as e:
            logger.warning(f"Unexpect error on exam exist deployment: {e}")
            return

        for model_name, model_monitor in model_monitors.items():
            if model_monitor.clear_outdated_deployments() == 0:
                logger.info(
                    f'No deployment exists any more in "{model_name}", stopping the model monitor.'
                )
                model_monitor.stop()
                del model_monitors[model_name]

        try:
            # Watch future deployments
            w = watch.Watch()
            signal["watch"] = w
            for event in w.stream(
                apps_v1.list_namespaced_deployment,
                namespace=NAMESPACE,
                label_selector=MODEL_LABEL,
                resource_version=watch_version,
                timeout_seconds=timeout,
            ):
                if signal["done"]:
                    return
                try:
                    deployment = event["object"]
                    if event["type"] == "ADDED":
                        start_serving_thread(watch_version, deployment, True)
                    elif event["type"] == "DELETED":
                        remove_deployment(deployment)
                except Exception as e:
                    logger.warning(
                        f"Error on handle event {event['type']} {deployment.metadata.name}: {e}"
                    )
        except client.rest.ApiException as ae:
            logger.warning(f"Error connecting to Kubernetes API: {ae}. Will retry.")
        except Exception as e:
            logger.warning(f"Unexpect error on watch deployment: {e}")
            return


if __name__ == "__main__":
    import sys

    if "--debug" in sys.argv:
        debug = True

    # Setup default logger
    logging.basicConfig(level=logging.DEBUG if debug else logging.INFO)
    logging.getLogger("kubernetes.client.rest").setLevel(
        logging.ERROR
    )  # Suppress kubenetes logs
    logging.getLogger("pulp.apis.core").setLevel(logging.INFO)  # Suppress pulp logs
    logger = logging.getLogger("aibrix.gpuoptimizer")

    timeout = 600
    try:
        config.load_incluster_config()
    except Exception:
        # Local debug
        config.load_kube_config(config_file="~/.kube/config")
    signal = {"done": False, "watch": None}
    threading.Thread(
        target=main,
        args=(
            signal,
            timeout,
        ),
    ).start()  # Run Kubernetes informer in a separate thread

    uvicorn.run(
        app,
        host="0.0.0.0",
        port=8080,
        log_config={
            "version": 1,
            "disable_existing_loggers": False,
            "loggers": {"uvicorn.access": {"filters": ["dash_update_filter"]}},
            "filters": {
                "dash_update_filter": {
                    "()": ExcludePathsFilter,
                    "exclude_paths": ["/dash/{model_name}/_dash"],  # Paths to exclude
                },
            },
        },
    )

    signal["done"] = True
    if signal["watch"] is not None:
        signal["watch"].stop()  # type: ignore

    # clean up
    for model_name, model_monitor in model_monitors.items():
        model_monitor.stop()
