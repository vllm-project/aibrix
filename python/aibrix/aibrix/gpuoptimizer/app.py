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

import threading
from starlette.applications import Starlette
from starlette.responses import JSONResponse, PlainTextResponse
import logging
import uvicorn
import os
from loadmonitor.monitor import ModelMonitor
try:
    from kubernetes import client, config, watch
except Exception as e:
    print(f"Failed to import kubernetes, skip: {e}")

NAMESPACE = os.getenv('NAMESPACE', 'aibrix-system')
MODEL_LABEL = "model.aibrix.ai/name"

app = Starlette()
model_monitors = {} # Dictionary to store serving threads

logger = logging.getLogger("uvicorn")
logger.setLevel(logging.DEBUG)

def start_serving_thread(deployment, new_deployment) -> bool:
    """Start model monitor, returns True if a new server thread is created, False otherwise."""
    logger.debug(f"Starting model monitor for deployment \"{deployment.metadata.name}\"")

    labels = deployment.metadata.labels
    if not labels:
        logger.warning(f"No labels found for this deployment, please specify \"{MODEL_LABEL}\" label in deployment.")
        return False
    # Access model label
    model_name = labels.get(MODEL_LABEL)
    if not model_name:
        logger.warning(f"No \"{MODEL_LABEL}\" label found for this deployment, please specify \"{MODEL_LABEL}\" label in deployment.")
        return False

    # Get deployment specs
    deployment_name = deployment.metadata.name
    namespace = deployment.metadata.namespace
    replicas = deployment.spec.replicas
    if not replicas:
        replicas = 1
    
    if model_name in model_monitors:
        model_monitor = model_monitors[model_name]
        model_monitor.add_deployment(deployment_name, namespace, replicas)
        logger.info(f"Deployment \"{deployment_name}\" added to the model monitor for \"{model_name}\"")
        return False

    model_monitor = ModelMonitor(model_name, deployment_name=deployment_name, namespace=namespace, replicas=replicas)
    thread = threading.Thread(target=model_monitor.start)
    thread.daemon = True
    model_monitor.thread = thread
    thread.start()
    model_monitors[model_name] = model_monitor
    if new_deployment:
        logger.info(f"New model monitor started for \"{model_name}\". Deployment \"{deployment_name}\" added.")
    else:
        logger.info(f"Model monitor started for existed \"{model_name}\". Deployment \"{deployment_name}\" added.")

def remove_deployment(deployment):
    """Remove deployment from model monitor"""
    labels = deployment.metadata.labels
    if not labels:
        logger.warning(f"No labels found for this deployment, please specify \"{MODEL_LABEL}\" label in deployment.")
        return
    # Access model label
    model_name = labels.get(MODEL_LABEL)
    if not model_name:
        logger.warning(f"No \"{MODEL_LABEL}\" label found for this deployment, please specify \"{MODEL_LABEL}\" label in deployment.")
        return

    deployment_name = deployment.metadata.name
    namespace = deployment.metadata.namespace
    if model_name not in model_monitors:
        logger.warning(f"Removing \"{deployment_name}\" from the model monitor, but \"{model_name}\" has not monitored.")
        return

    if model_monitors[model_name].remove_deployment(deployment_name, namespace) == 0:
        model_monitors[model_name].stop()
        del model_monitors[model_name]
        logger.info(f"Removing \"{deployment_name}\" from the model monitor, no deployment left in \"{model_name}\", stopping the model monitor.")
        return

    logger.info(f"Removing \"{deployment_name}\" from the model monitor for \"{model_name}\".")

@app.route('/monitor/{namespace}/{deployment_name}', methods=['POST'])
async def start_deployment_optimization(request):
    namespace = request.path_params['namespace']
    deployment_name = request.path_params['deployment_name']
    try:
        # Verify the deployment exists
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        # Start the deployment optimization
        if start_serving_thread(deployment, True):
            return JSONResponse({"message": "Deployment optimization started"})
        else:
            return JSONResponse({"message": "Deployment optimization already started"})
    except Exception as e:
        return JSONResponse({"error": f"Error starting deployment optimization: {e}"}, status_code=500)
    
@app.route('/scale/{namespace}/{deployment_name}/{replicas}', methods=['POST'])
async def start_deployment_optimization(request):
    namespace = request.path_params['namespace']
    deployment_name = request.path_params['deployment_name']
    replicas = request.path_params['deployment_name']
    try:
        # Verify the deployment exists
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)

        # Start the deployment optimization
        if start_serving_thread(deployment):
            return JSONResponse({"message": "Deployment optimization started"})
        else:
            return JSONResponse({"message": "Deployment optimization already started"})
    except Exception as e:
        return JSONResponse({"error": f"Error starting deployment optimization: {e}"}, status_code=500)

@app.route('/metrics/{namespace}/{deployment_name}')
async def get_deployment_metrics(request):
    namespace = request.path_params['namespace']
    deployment_name = request.path_params['deployment_name']
    # get deployment information
    try:
        apps_v1 = client.AppsV1Api()
        deployment = apps_v1.read_namespaced_deployment(deployment_name, namespace)
        labels = deployment.metadata.labels
        if not labels:
            raise Exception(f"No labels found for this deployment")
        
        # Access model label
        model_name = labels.get(MODEL_LABEL)
        if not model_name:
            raise Exception(f"No \"{MODEL_LABEL}\" label found for this deployment")
        
        if model_name not in model_monitors:
            raise Exception(f"The model {model_name} is not being monitored")
        monitor = model_monitors[model_name]

        replicas = monitor.read_deployment_num_replicas(deployment_name, namespace)
    except Exception as e:
        return JSONResponse({"error": f"Failed to read metrics: {e}"}, status_code=404)

    # construct Prometheus-style Metrics
    metrics_output = f"""# HELP vllm:deployment_replicas Number of suggested replicas.
# TYPE vllm:deployment_replicas gauge
vllm:deployment_replicas{{model_name="{model_name}"}} {replicas}
"""
    return PlainTextResponse(metrics_output)

def main():
    try:
        apps_v1 = client.AppsV1Api()

        # List existing deployments
        logger.info(f"Looking for deployments in {NAMESPACE} with {MODEL_LABEL}")
        deployments = apps_v1.list_namespaced_deployment(namespace=NAMESPACE, label_selector=MODEL_LABEL)
        resource_version = deployments.metadata.resource_version
        for deployment in deployments.items:
            start_serving_thread(deployment, False)
    except client.rest.ApiException as ae:
        logger.error(f"Error connecting to Kubernetes API: {ae}. Please manually initiate GPU optimizer by calling the /monitor/{{namespace}}/{{deployment_name}} endpoint")
        return
    except Exception as e:
        logger.warning(f"Unexpect error on exam exist deployment: {e}")
        return
    
    while True:
        try:
            # Watch future deployments
            w = watch.Watch()
            for event in w.stream(apps_v1.list_namespaced_deployment,
                                    namespace=NAMESPACE,
                                    label_selector=MODEL_LABEL,
                                    resource_version=resource_version):
                try:
                    deployment = event['object']
                    resource_version = deployment.metadata.resource_version
                    if event['type'] == 'ADDED':
                        start_serving_thread(deployment, True) 
                    elif event['type'] == 'DELETED':
                        remove_deployment(deployment)
                except Exception as e:
                    logger.warning(f"Error on handle event {event}: {e}")

        except client.rest.ApiException as ae:
            logger.warning(f"Error connecting to Kubernetes API: {ae}. Will retry.")
        except Exception as e:
            logger.warning(f"Unexpect error on watch deployment: {e}")
            return
        
    
if __name__ == '__main__':
    config.load_incluster_config()
    threading.Thread(target=main).start()  # Run Kubernetes informer in a separate thread
    uvicorn.run(app, host='0.0.0.0', port=8080)