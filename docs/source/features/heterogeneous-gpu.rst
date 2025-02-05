.. _heterogeneous-gpu:

============================
Heterogeneous GPU Inference
============================

Heterogeneous GPU Inference is a feature that enables users to utilize different types of GPUs for deploying the same model. This feature addresses two primary challenges associated with Large Language Model (LLM) inference: (1) As the demand for large-scale model inference increases, ensuring consistent GPU availability has become a challenge, particularly within regions where identical GPU types are often unavailable due to capacity constraints. (2) Users may seek to incorporate lower-cost, lower-performance GPUs to reduce overall expenses. 

Design Overview
---------------

There are three main components in Heterogeneous GPU Inference Feature: (1) LLM Request Monitoring, (2) Heterogeneous GPU Optimizer, (3) Request Routing. The following figure shows the overall architecture. First, LLM Request Monitoring component is responsible for monitoring the past inference requests and their request patterns. Second, Heterogeneous GPU Optimizer component is responsible for selecting the optimal GPU type and the corresponding GPU count. Third, Request Routing component is responsible for routing the request to the optimal GPU.

.. figure:: ../assets/images/heterogeneous-gpu-diagram.png
  :alt: heterogeneous-gpu-diagram
  :width: 100%
  :align: center


Example
-------

Step 1: Deploy the heterogeneous deployments.

One deployment and corresponding PodAutoscaler should be deployed for each GPU type.
See `sample heterogeneous configuration <https://github.com/aibrix/aibrix/tree/main/samples/heterogeneous>`_ for an example of heterogeneous configuration composed of two GPU types. The following codes 
deploy heterogeneous deployments using L20 and A10 GPU.

.. code-block:: bash

    kubectl apply -k samples/heterogeneous

After deployment, you will see a inference service with two pods running on simulated L20 and A10 GPUs:

.. code-block:: bash

    kubectl get svc
    NAME         TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)          AGE
    kubernetes   ClusterIP   10.96.0.1       <none>        443/TCP          14d
    [model_name] NodePort    10.107.122.88   <none>        8000:30081/TCP   48m

Incoming requests are routed through the gateway and directed to the optimal pod based on request patterns:

.. code-block:: bash

    kubectl get pods
    NAME                               READY   STATUS        RESTARTS      AGE
    [model_name]-a10-5c9576c566-jfblm  2/2     Running       0             27s
    [model_name]-l20-9bdfbb7ff-rx9r7   2/2     Running       0             46m

Step 2: Install aibrix python module:

.. code-block:: bash

    pip3 install aibrix

.. note::

  The GPU Optimizer runs continuously in the background, dynamically adjusting GPU allocation for each model based on workload patterns.
  Note that GPU optimizer requires offline inference performance benchmark data for each type of GPU on each specific LLM model.

.. If local heterogeneous deployments is used, you can find the prepared benchmark data under `python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/ <https://github.com/aibrix/aibrix/tree/main/python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/>`_ and skip Step 3. See :ref:`Development` for details on deploying a local heterogeneous deployments.

Step 3: Benchmark model. For each type of GPU, run ``aibrix_benchmark``. See `benchmark.sh <https://github.com/aibrix/aibrix/tree/main/python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/benchmark.sh>`_ for more options.

.. code-block:: bash

    kubectl port-forward [pod_name] 8010:8000 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    aibrix_benchmark -m [model_name] -o [path_to_benchmark_output]

Step 4: Decide SLO and generate profile, run `aibrix_gen_profile -h` for help.
  
.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    aibrix_gen_profile [deploy_name1] --cost [cost1] [SLOs] -o "redis://localhost:6379/?model=[model_name]"
    aibrix_gen_profile [deploy_name2] --cost [cost2] [SLOs] -o "redis://localhost:6379/?model=[model_name]"

Now that the GPU Optimizer is ready to work. You should observe that the number of workload pods changes in response to the requests sent to the gateway.

.. note::

  Requests must be routed through gateway for the GPU optimizer to work as expected.

Miscellaneous
-------------

Once the GPU optimizer finishes the scaling optimization, the output of the GPU optimizer is passed to PodAutoscaler as a metricSource via a designated HTTP endpoint for the final scaling decision.  In the above local a100 and a40 deployment files, we configure the PodAutoscaler spec (using a40 as an example).

.. code-block:: yaml

    apiVersion: autoscaling.aibrix.ai/v1alpha1
    kind: PodAutoscaler
    metadata:
      name: podautoscaler-[model_name]-a10
      labels:
        app.kubernetes.io/name: aibrix
        app.kubernetes.io/managed-by: kustomize
        kpa.autoscaling.aibrix.ai/scale-down-delay: 0s
      namespace: default
    spec:
      scaleTargetRef:
        apiVersion: apps/v1
        kind: Deployment
        name: [model_name]-a10 # replace with corresponding deployment name
      minReplicas: 0
      maxReplicas: 10
      metricsSources: 
        - metricSourceType: domain
          protocolType: http
          endpoint: aibrix-gpu-optimizer.aibrix-system.svc.cluster.local:8080
          path: /metrics/default/[model_name]-a10 # replace with /metrics/default/[deployment name]
          targetMetric: "vllm:deployment_replicas"
          targetValue: "1"
      scalingStrategy: "KPA"

To avoiding scaling down to 0 workload pod when there is no workload, a new label  ``model.aibrix.ai/min_replicas`` in the deployment file is used to specify the minimum number of replicas.

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: [model_name]-a10
      labels:
        model.aibrix.ai/name: "[model_name]"
        model.aibrix.ai/min_replicas: "1" # min replica for gpu optimizer when no workloads.
    ... rest yaml deployments