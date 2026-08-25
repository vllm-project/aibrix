.. _optimizer-based-autoscaling:

==========================
Optimizer-based Autoscaler
==========================

Overview
--------

Metric-based autoscalers react: they wait for a metric such as queue depth to cross a threshold,
then add replicas. The optimizer-based autoscaler plans instead. It combines an offline benchmark
of how each GPU type performs on a model with a latency or throughput SLO, watches the actual mix
of request sizes arriving at the gateway, and computes how many replicas are needed to serve that
mix within the SLO at the lowest cost. The result is handed to a ``PodAutoscaler`` as a metric,
so the final scaling action still goes through the same controller as every other strategy.

Because it reasons about GPU capacity rather than a single utilisation number, it is also the
mechanism behind :doc:`../heterogeneous-gpu`, where several GPU types serve one model and the
optimizer decides how many of each to run.

How it works
------------

.. mermaid::

   graph LR
       GW["Gateway plugins<br/>request tracing"] -->|"request traces"| R["Redis"]
       P["aibrix_gen_profile<br/>(offline)"] -->|"GPU profiles"| R
       R --> O["GPU optimizer"]
       O -->|"/metrics/ns/deployment<br/>vllm:deployment_replicas"| PA["PodAutoscaler<br/>external metric source"]
       PA -->|scales| D["Deployment"]
       O -.watches.-> D

1. **Request tracing.** With ``AIBRIX_GPU_OPTIMIZER_TRACING_FLAG=true`` on the gateway plugin,
   every request's token statistics are recorded in Redis, giving the optimizer the live
   distribution of input and output lengths per model.
2. **Profiles.** ``aibrix_benchmark`` measures a deployment on one GPU type across input and
   output length patterns. ``aibrix_gen_profile`` turns that benchmark into a capacity profile
   for a chosen SLO and GPU cost and stores it in Redis under the model's name. One profile per
   GPU type.
3. **Optimization.** The GPU optimizer (``aibrix-gpu-optimizer`` in ``aibrix-system``) watches
   ``Deployment`` objects that carry the ``model.aibrix.ai/name`` label, loads the profiles for
   that model, and solves for the replica count per deployment that covers the observed request
   mix within the SLO at minimum cost.
4. **Scaling.** The recommendation is exposed as the Prometheus metric
   ``vllm:deployment_replicas`` at ``/metrics/<namespace>/<deployment>``. A ``PodAutoscaler``
   with an ``external`` metric source (``domain`` in the sample, a deprecated alias) consumes
   it.

Prerequisites
-------------

* The GPU optimizer is part of the default AIBrix install (``config/default`` includes
  ``config/gpu-optimizer``). Confirm it is running: ``kubectl get deploy -n aibrix-system
  aibrix-gpu-optimizer``.
* Request tracing is **off** by default. Enable it by redeploying the gateway plugin with the
  experimental overlay, or by adding the environment variable yourself:

  .. code-block:: bash

      kubectl apply -k config/experimentals/gpu-optimizer
      # or: kubectl edit deployment aibrix-gateway-plugins -n aibrix-system
      #     and add AIBRIX_GPU_OPTIMIZER_TRACING_FLAG=true to the gateway-plugin container

* The ``aibrix`` Python package on the machine where you run the benchmark:
  ``pip3 install aibrix``. The profiling tools additionally need ``tiktoken`` and
  ``transformers``.
* Access to the AIBrix Redis instance from that machine (a ``port-forward`` is enough).

Step 1: Benchmark the deployment
--------------------------------

For each type of GPU, run ``aibrix_benchmark``. See `benchmark.sh <https://github.com/vllm-project/aibrix/tree/main/python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/benchmark.sh>`_ for more options.

.. code-block:: bash

    kubectl port-forward [pod_name] 8010:8000 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    aibrix_benchmark -m deepseek-llm-7b-chat -o [path_to_benchmark_output]

Step 2: Decide the SLO and generate the profile
-----------------------------------------------

Run ``aibrix_gen_profile -h`` for help. The first argument is the profile name; the samples use
``<model>-<gpu>`` so that one model can have one profile per GPU type.

.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    aibrix_gen_profile deepseek-llm-7b-chat-v100 --cost [cost1] [SLO-metric] [SLO-value] -o "redis://localhost:6379/?model=deepseek-llm-7b-chat"

Now the GPU Optimizer is ready to work. Once it has enough trace data it reloads the profiles on
its own; to force a reload right away:

.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-gpu-optimizer 8080:8080 1>/dev/null 2>&1 &
    curl http://localhost:8080/update_profile/deepseek-llm-7b-chat

Step 3: Deploy the PodAutoscaler
--------------------------------

It is simply a matter of applying the podautoscaler yaml file. The GPU optimizer exposes custom metrics which can be used by podautoscalers to make scaling decisions as explained above. One important thing you should note is that the deployment name and the name in scaleTargetRef in PodAutoscaler must be the same. That's how AIBrix PodAutoscaler refers to the right deployment.

All the sample files can be found in the following directory.

.. code-block:: bash

    https://github.com/vllm-project/aibrix/tree/main/samples/autoscaling

Example Optimizer-based KPA yaml config
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../../samples/autoscaling/optimizer-kpa.yaml
   :language: yaml

The part that makes this an optimizer-driven autoscaler is the ``metricsSources`` entry
pointing at the optimizer's service with ``targetMetric: vllm:deployment_replicas``;
``minReplicas`` and ``maxReplicas`` bound what the optimizer may request.

.. note::

   Two details of this sample do not match the current controller, so check them against the
   AIBrix version you run:

   * The ``kpa.autoscaling.aibrix.ai/scale-down-delay`` annotation is not read by the
     controller. The supported key for shortening scale-down is
     ``autoscaling.aibrix.ai/scale-down-cooldown-window`` (default ``300s``), listed in
     :doc:`metric-based-autoscaling`.
   * The external metric fetcher requests ``http://<endpoint>/metrics`` regardless of ``path``
     and looks ``targetMetric`` up in AIBrix's metric registry, which does not currently list
     ``vllm:deployment_replicas``.

Verify
------

The optimizer-based autoscaler decides the number of GPUs based on the offline GPU capacity profiling. It proactively calculates the overall capacity needed for serving requests under SLO and ensures that the GPU capacity is fully used but not overloaded. The GPU optimizer's output is exposed as custom metrics. The following shows how these custom metrics can be checked.

.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-gpu-optimizer 8080:8080

    curl http://localhost:8080/metrics/default/deepseek-llm-7b-chat-v100
    # HELP vllm:deployment_replicas Number of suggested replicas.
    # TYPE vllm:deployment_replicas gauge
    vllm:deployment_replicas{model_name="deepseek-llm-7b-chat"} 1

You should observe that the number of workload pods changes in response to the requests sent to
the gateway.

GPU optimizer logs
^^^^^^^^^^^^^^^^^^

Gpu optimizer is an individual component that plays the role of collecting metrics from each pod. You can check its logs in this way. ``kubectl logs <aibrix-gpu-optimizer-podname> -n aibrix-system -f``

.. code-block:: bash

    {"time": "2025-02-12 06:23:52,086", "level": "INFO", "logger": "aibrix.gpu_optimizer.load_monitor", "message": "deepseek-llm-7b-chat optimization took 6.660938262939453 ms, cost $51.3324, coverage: 72.62180974477958%: [deepseek-llm-7b-chat-v100: 2($51.3324)]"}

In the above logs, the GPU optimizer returns the number of GPUs suggested, which is 2 in this example.

Configuration reference
-----------------------

**GPU optimizer deployment** (``aibrix-gpu-optimizer``, ``aibrix-system``). It runs
``python -m aibrix.gpu_optimizer.app`` and listens on port ``8080``.

.. list-table::
   :header-rows: 1
   :widths: 30 30 40

   * - Environment variable
     - Default
     - Meaning
   * - ``REDIS_HOST``
     - ``localhost`` (the manifest sets ``aibrix-redis-master.aibrix-system.svc.cluster.local``)
     - Redis that holds request traces and profiles.
   * - ``REDIS_PORT``
     - ``6379``
     - Redis port.
   * - ``REDIS_PASSWORD``
     - *(unset)*
     - Redis password, if any.

**Gateway plugin**: ``AIBRIX_GPU_OPTIMIZER_TRACING_FLAG`` (default ``false``) turns request
tracing on.

**Deployment labels** read by the optimizer:

.. list-table::
   :header-rows: 1
   :widths: 36 64

   * - Label
     - Meaning
   * - ``model.aibrix.ai/name``
     - Required. Deployments without it are ignored, and the value must match the ``model``
       query parameter used when the profile was stored.
   * - ``model.aibrix.ai/min_replicas``
     - Replicas to keep when there is no traffic at all. Defaults to ``0``. Ignored while
       requests are flowing. Keep at least one deployment per model at ``"1"`` so a ready pod
       always exists.

**PodAutoscaler**: use ``scalingStrategy: KPA`` and an ``external`` metric source (the sample
still says ``domain``, a deprecated alias that behaves the same) whose
``endpoint`` is ``aibrix-gpu-optimizer.aibrix-system.svc.cluster.local:8080``, ``path`` is
``/metrics/<namespace>/<deployment>`` and ``targetMetric`` is ``vllm:deployment_replicas``. Set
``minReplicas`` to ``0`` when the optimizer should be free to turn a deployment off; a higher
value overrides the recommendation. See :doc:`autoscaling` for the remaining fields.

**aibrix_gen_profile**

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Argument
     - Meaning
   * - ``deployment`` (positional)
     - Profile name. Use the deployment name of the GPU type being profiled.
   * - ``--benchmark``
     - Benchmark result file produced by ``aibrix_benchmark``.
   * - ``--tput``, ``--tt``
     - Throughput SLO targets: requests per second, and tokens per second.
   * - ``--e2e``, ``--ttft``, ``--tpat``, ``--tpot``
     - Latency SLO targets in seconds: end-to-end, time to first token, time per all tokens,
       time per output token. Set whichever ones your SLO is defined on.
   * - ``--percentile``
     - ``0`` (mean, default), ``50``, ``90`` or ``99``. Which percentile of the benchmark the
       SLO is checked against.
   * - ``--cost``
     - Relative cost of this GPU type, default ``1.0``. The optimizer minimises total cost, so
       only the ratio between GPU types matters.
   * - ``-o``
     - Output. A file path, or ``redis://[user:password@]host:port[/db]?model=<model>`` to
       store the profile where the optimizer reads it.
   * - ``-v``
     - Print details of the generated profile.

**HTTP endpoints** on the optimizer:

.. list-table::
   :header-rows: 1
   :widths: 44 56

   * - Endpoint
     - Purpose
   * - ``GET /metrics/{namespace}/{deployment}``
     - Prometheus metrics for one deployment, including ``vllm:deployment_replicas``.
   * - ``GET /update_profile/{model}``
     - Reload profiles for a model from Redis.
   * - ``POST`` / ``DELETE /monitor/{namespace}/{deployment}``
     - Start or stop monitoring a deployment by hand. Normally unnecessary: the optimizer
       discovers labelled deployments itself.
   * - ``PUT /scale/{namespace}/{deployment}``
     - Force a replica count for a monitored deployment.
   * - ``GET /dash/{model}``
     - Dashboard visualising the observed workload pattern for a model.

Preliminary experiments with different autoscalers
--------------------------------------------------

Here we show the preliminary experiment results to show how different autoscaling mechanisms and configurations for autoscalers affect performance(latency) and cost (compute cost).

- Set up
    - Model: Deepseek 7B chatbot model
    - GPU type: V100
    - Max number of GPU: 8
    - HPA, KPA, and APA use metrics as the scaling metrics: 70.
    - Optimizer-based KPA SLO: E2E P99 100s
- Workload
    - The overall RPS trend starts with low RPS and goes up relatively fast until T=500 to evaluate how different autoscaler and config reacts to the rapid load increase. After that, it goes down to low RPS quickly to evaluate scaling down behavior and goes up again slowly.
        - Average RPS trend: 0.5 RPS -> 2 RPS -> 4 RPS -> 5 RPS -> 1 RPS -> 3 RPS


Experiments Results
^^^^^^^^^^^^^^^^^^^

- gpu_cache_usage_perc: 70

.. image:: ../../assets/images/autoscaler/optimizer-based-autoscaling-70-results.png
   :alt: result
   :width: 720px
   :align: center
