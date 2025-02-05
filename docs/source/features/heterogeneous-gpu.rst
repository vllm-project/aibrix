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

    kubectl apply -f samples/heterogeneous

After deployment, you will see a inference service with two pods running on simulated L20 and A10 GPUs:

.. code-block:: bash

    kubectl get svc
    NAME                TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)          AGE
    deepseek-coder-7b   NodePort    10.102.95.136   <none>        8000:30081/TCP   2s
    kubernetes          ClusterIP   10.96.0.1       <none>        443/TCP          54d

Incoming requests are routed through the gateway and directed to the optimal pod based on request patterns:

.. code-block:: bash

    kubectl get pods
    NAME                                       READY   STATUS    RESTARTS   AGE
    deepseek-coder-7b-a10-96667667c-6gjql      2/2     Running   0          33s
    deepseek-coder-7b-l20-96667667c-7zj7k      2/2     Running   0          33s


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
    aibrix_benchmark -m deepseek-coder-7b -o [path_to_benchmark_output]

Step 4: Decide SLO and generate profile, run `aibrix_gen_profile -h` for help.
  
.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    aibrix_gen_profile deepseek-coder-7b-a10 --cost [cost1] [SLO-metric] [SLO-value] -o "redis://localhost:6379/?model=deepseek-coder-7b"
    aibrix_gen_profile deepseek-coder-7b-l20 --cost [cost2] [SLO-metric] [SLO-value] -o "redis://localhost:6379/?model=deepseek-coder-7b"

Now that the GPU Optimizer is ready to work. You should observe that the number of workload pods changes in response to the requests sent to the gateway.

.. note::

  Requests must be routed through gateway for the GPU optimizer to work as expected.

Miscellaneous
-------------

Once the GPU optimizer finishes the scaling optimization, the output of the GPU optimizer is passed to PodAutoscaler as a metricSource via a designated HTTP endpoint for the final scaling decision.  The following is an example of PodAutoscaler spec.

.. code-block:: yaml

    apiVersion: autoscaling.aibrix.ai/v1alpha1
    kind: PodAutoscaler
    metadata:
      name: podautoscaler-deepseek-coder-7b-a10
      labels:
        app.kubernetes.io/name: aibrix
        app.kubernetes.io/managed-by: kustomize
        kpa.autoscaling.aibrix.ai/scale-down-delay: 0s
      namespace: default
    spec:
      scaleTargetRef:
        apiVersion: apps/v1
        kind: Deployment
        name: deepseek-coder-7b-a10 # replace with corresponding deployment name
      minReplicas: 1 # configure min replica or gpu optimizer will scale down to 0 when no workloads.
      maxReplicas: 10 # replace with max number of nodes in the cluster
      metricsSources: 
        - metricSourceType: domain
          protocolType: http
          endpoint: aibrix-gpu-optimizer.aibrix-system.svc.cluster.local:8080
          path: /metrics/default/deepseek-coder-7b-a10 # replace with /metrics/default/[deployment name]
          targetMetric: "vllm:deployment_replicas"
          targetValue: "1"
      scalingStrategy: "KPA"

