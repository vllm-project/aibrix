.. _ai_runtime:

=================
AI Engine Runtime
=================

Overview
--------

The AI Engine Runtime is a small HTTP service that runs next to the inference engine, usually as
a sidecar container in the same pod. It gives the AIBrix control plane one stable API for things
that otherwise differ from engine to engine:

* **Metric standardization**: it scrapes the engine's ``/metrics`` and re-exports them on its
  own port with a consistent naming, so the autoscaler and the gateway can read one shape.
* **Model and adapter management**: it downloads model weights from HuggingFace, S3 or TOS and
  loads or unloads LoRA adapters on the engine. The :doc:`lora-dynamic-loading` controller
  calls these endpoints.
* **Runtime model lifecycle**: an experimental set of endpoints used by :doc:`modelclaim` to
  start, sleep, wake and deactivate engine processes in a shared pod.

The runtime does not proxy inference traffic: Envoy forwards requests straight to the engine
container. The only contact the gateway has with the runtime is the wake call it makes for a
sleeping ModelClaim engine. Most deployments do not need the runtime. Install it when you use
dynamic LoRA loading or ModelClaim.

How it works
------------

.. mermaid::

   graph LR
       CP["Control plane<br/>ModelAdapter / ModelClaim controllers"] -->|":8080 HTTP"| RT["aibrix-runtime<br/>sidecar"]
       PROM["Prometheus / autoscaler"] -->|"/metrics :8080"| RT
       RT -->|"/metrics, /v1/load_lora_adapter, ..."| ENG["Engine<br/>:8000"]
       subgraph POD["Engine pod"]
           RT
           ENG
       end

* The runtime listens on port ``8080`` and reaches the engine through
  ``INFERENCE_ENGINE_ENDPOINT`` (``http://localhost:8000`` by default).
* The controller manager flag ``--enable-runtime-sidecar`` only affects the ModelAdapter
  (LoRA) controller. With it on, that controller uses the runtime API on ``8080`` when a
  container named ``aibrix-runtime`` is present and the engine's own API on ``8000`` otherwise;
  with it off (the default) it always calls the engine. The ModelClaim controller and the
  gateway's wake path always use the runtime on ``8080``, regardless of the flag.
* Metrics are collected on each scrape: the runtime fetches the engine's metrics page, applies
  the standardization rules for ``INFERENCE_ENGINE`` and serves the result. Rule sets for
  ``sglang`` and ``trtllm`` exist in the code, but the runtime only starts with
  ``INFERENCE_ENGINE=vllm``: the engine client it initialises at startup rejects every other
  value.
* The model management API (``/v1/lora_adapter/*``, ``/v1/models``) is implemented for vLLM
  0.6.1 and later. Other engines are not supported by those endpoints yet.

Installation
------------

AIBrix Runtime can be injected into your workloads automatically using webhook-based sidecar injection (recommended) or manually added to your deployment manifests.

Automatic Sidecar Injection (Recommended)
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The easiest way to enable the runtime is through automatic sidecar injection. Simply add an annotation to your Deployment or StormService:

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: vllm-server
      annotations:
        model.aibrix.ai/sidecar-injection: "true"  # Enable automatic runtime injection
    spec:
      template:
        spec:
          containers:
          - name: vllm
            image: vllm/vllm-openai:latest
            # Your container configuration...

The webhook will automatically inject the ``aibrix-runtime`` sidecar container into your pods.

What gets injected, so you know what to expect in the pod spec:

* a container named ``aibrix-runtime`` running ``aibrix_runtime --port 8080`` from image
  ``aibrix/runtime:v0.5.0`` unless overridden by the image annotation below;
* ``INFERENCE_ENGINE`` taken from the ``model.aibrix.ai/engine`` annotation on the workload
  when set, otherwise inferred from the engine container's image name (``vllm``, ``sglang``,
  ``tgi``, ``triton``, ``llamacpp``, else ``unknown``), and
  ``INFERENCE_ENGINE_ENDPOINT=http://localhost:8000``. Only ``vllm`` yields a runtime that
  starts, so inject it only into vLLM workloads;
* a container port named ``metrics`` on ``8080``, a liveness probe on ``/healthz`` and a
  readiness probe on ``/ready``;
* a volume ``adapter-storage`` mounted at ``/tmp/aibrix/adapters`` for downloaded adapters;
* resource requests of ``100m`` CPU and ``256Mi`` memory, limits of ``500m`` and ``512Mi``.

The webhook is registered for ``Deployment`` and ``StormService`` objects with
``failurePolicy: Ignore``, so a webhook outage never blocks workload creation; it only means
the sidecar is not added.

**For StormService**

Sidecar injection also works with StormService custom resources:

.. code-block:: yaml

    apiVersion: orchestration.aibrix.ai/v1alpha1
    kind: StormService
    metadata:
      name: my-service
      annotations:
        model.aibrix.ai/sidecar-injection: "true"
    spec:
      template:
        spec:
          roles:
          - name: worker
            template:
              spec:
                containers:
                - name: vllm
                  image: vllm/vllm-openai:latest
                  # ...

The runtime sidecar will be injected into each role's pod template.

**Customize Runtime Image**

You can specify a custom runtime image using an annotation:

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: vllm-server
      annotations:
        model.aibrix.ai/sidecar-injection: "true"
        model.aibrix.ai/sidecar-runtime-image: "aibrix/runtime:v0.5.0"  # Custom image
    spec:
      # ...

**Enable Global Runtime Flag**

To enable the controller to use the runtime sidecar API, set the global flag when starting the controller manager:

.. code-block:: bash

    # Enable runtime sidecar globally
    ./bin/controller-manager --enable-runtime-sidecar=true

The runtime detection logic works as follows:

- **EnableRuntimeSidecar = false**: Controller always uses direct engine API (port 8000), even if sidecar is injected
- **EnableRuntimeSidecar = true**: Controller detects if pod has ``aibrix-runtime`` container:

  - Sidecar present → Uses runtime API (port 8080)
  - Sidecar absent → Fallback to direct engine API (port 8000)

This design ensures functionality works with or without the runtime sidecar, providing maximum flexibility.

Manual Sidecar Installation
^^^^^^^^^^^^^^^^^^^^^^^^^^^

If you prefer manual control, you can add the runtime sidecar directly to your deployment YAML:

.. code-block:: yaml

      containers:
      - name: vllm
        image: vllm/vllm-openai:latest
        # Your main container configuration...

      - name: aibrix-runtime
        image: aibrix/runtime:v0.5.0
        command:
        - aibrix_runtime
        - --port
        - "8080"
        env:
        - name: INFERENCE_ENGINE
          value: "vllm"  # only vllm is supported
        - name: INFERENCE_ENGINE_ENDPOINT
          value: "http://localhost:8000"
        ports:
        - containerPort: 8080
          protocol: TCP
        volumeMounts:
        - mountPath: /models
          name: model-hostpath
      volumes:
      - name: model-hostpath
        hostPath:
          path: /root/models
          type: DirectoryOrCreate

Standalone Installation
^^^^^^^^^^^^^^^^^^^^^^^

If you like to use the runtime for other cases outside of Kubernetes, you can install it by the following command.

.. attention:: 

    ``python3 -m pip install aibrix``

    If you want to use nightly version, you can install from code.

    ``cd $AIBRIX_HOME/python/aibrix && python3 -m pip install -e .``


Metric Standardization
----------------------

Different inference engines will expose different metrics, and AI Runtime will standardize them.

Define the information related to the inference engine side in the container environment variables. For example, if ``vLLM`` provides metrics services on ``http://localhost:8000/metrics``, launch the AI Runtime Server by the following command:

.. code-block:: bash

    INFERENCE_ENGINE=vllm INFERENCE_ENGINE_ENDPOINT="http://localhost:8000" aibrix_runtime --port 8080


The runtime serves the result on ``http://localhost:8080/metrics``. Every metric the engine
exposes is passed through unchanged, and for the metrics that have a standardization rule the
runtime additionally emits a copy under an engine-neutral ``aibrix:`` name. The SGLang and
TRT-LLM columns below describe rule sets present in the code; they cannot be selected today
because the runtime only starts with ``INFERENCE_ENGINE=vllm``.

.. list-table::
   :header-rows: 1
   :widths: 31 23 23 23

   * - Standard name
     - vLLM source
     - SGLang source
     - TRT-LLM source
   * - ``aibrix:queue_size``
     - ``vllm:num_requests_waiting``
     - ``sglang:num_queue_reqs``
     - N/A
   * - ``aibrix:gpu_cache_usage_perc``
     - ``vllm:gpu_cache_usage_perc``
     - N/A
     - N/A
   * - ``aibrix:kv_cache_usage_perc``
     - ``vllm:kv_cache_usage_perc``
     - N/A
     - ``kv_cache_utilization``
   * - ``aibrix:token_usage``
     - N/A
     - ``sglang:token_usage``
     - N/A
   * - ``aibrix:prompt_tokens_total``
     - ``vllm:prompt_tokens_total``
     - ``sglang:prompt_tokens_total``
     - N/A
   * - ``aibrix:generation_tokens_total``
     - ``vllm:generation_tokens_total``
     - ``sglang:generation_tokens_total``
     - N/A
   * - ``aibrix:generation_throughput``
     - N/A
     - ``sglang:gen_throughput``
     - N/A
   * - ``aibrix:time_to_first_token_seconds``
     - ``vllm:time_to_first_token_seconds``
     - ``sglang:time_to_first_token_seconds``
     - ``time_to_first_token_seconds``
   * - ``aibrix:time_per_output_token_seconds``
     - ``vllm:time_per_output_token_seconds``
     - ``sglang:time_per_output_token_seconds``
     - ``time_per_output_token_seconds``
   * - ``aibrix:e2e_request_latency_seconds``
     - ``vllm:e2e_request_latency_seconds``
     - ``sglang:e2e_request_latency_seconds``
     - ``e2e_request_latency_seconds``
   * - ``aibrix:request_success_total``
     - ``vllm:request_success_total``
     - N/A
     - ``request_success_total``
   * - ``aibrix:cache_hit_rate``
     - N/A
     - ``sglang:cache_hit_rate``
     - N/A
   * - ``aibrix:kv_cache_hit_rate``
     - N/A
     - N/A
     - ``kv_cache_hit_rate``

Set ``METRICS_RAW_PASSTHROUGH_MODE=1`` (or ``METRICS_ENABLE_TRANSFORMATION=0``) to skip the
copies and serve the engine's metrics exactly as they are. If a rule fails on a scrape, the
runtime logs the error and falls back to raw passthrough for that scrape rather than dropping
metrics. A sample of the vLLM output as served by the runtime:


.. code-block:: bash

    # TYPE vllm:cache_config_info gauge
    vllm:cache_config_info{block_size="16",cache_dtype="auto",calculate_kv_scales="False",cpu_offload_gb="0",enable_prefix_caching="False",gpu_memory_utilization="0.9",is_attention_free="False",num_cpu_blocks="9362",num_gpu_blocks="81767",num_gpu_blocks_override="None",sliding_window="None",swap_space_bytes="4294967296"} 1.0
    # HELP vllm:num_requests_running Number of requests currently running on GPU.
    # TYPE vllm:num_requests_running gauge
    vllm:num_requests_running{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:num_requests_swapped Number of requests swapped to CPU.
    # TYPE vllm:num_requests_swapped gauge
    vllm:num_requests_swapped{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:num_requests_waiting Number of requests waiting to be processed.
    # TYPE vllm:num_requests_waiting gauge
    vllm:num_requests_waiting{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:gpu_cache_usage_perc GPU KV-cache usage. 1 means 100 percent usage.
    # TYPE vllm:gpu_cache_usage_perc gauge
    vllm:gpu_cache_usage_perc{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:cpu_cache_usage_perc CPU KV-cache usage. 1 means 100 percent usage.
    # TYPE vllm:cpu_cache_usage_perc gauge
    vllm:cpu_cache_usage_perc{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:cpu_prefix_cache_hit_rate CPU prefix cache block hit rate.
    # TYPE vllm:cpu_prefix_cache_hit_rate gauge
    vllm:cpu_prefix_cache_hit_rate{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} -1.0
    # HELP vllm:gpu_prefix_cache_hit_rate GPU prefix cache block hit rate.
    # TYPE vllm:gpu_prefix_cache_hit_rate gauge
    vllm:gpu_prefix_cache_hit_rate{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} -1.0
    # HELP vllm:lora_requests_info Running stats on lora requests.
    # TYPE vllm:lora_requests_info gauge
    vllm:lora_requests_info{max_lora="0",running_lora_adapters="",waiting_lora_adapters=""} 1.7382173358407154e+09
    # HELP vllm:num_preemptions_total Cumulative number of preemption from the engine.
    # TYPE vllm:num_preemptions_total counter
    vllm:num_preemptions_total{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 0.0
    # HELP vllm:prompt_tokens_total Number of prefill tokens processed.
    # TYPE vllm:prompt_tokens_total counter
    vllm:prompt_tokens_total{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 148.0
    # HELP vllm:generation_tokens_total Number of generation tokens processed.
    # TYPE vllm:generation_tokens_total counter
    vllm:generation_tokens_total{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 955.0
    # HELP vllm:request_success_total Count of successfully processed requests.
    # TYPE vllm:request_success_total counter
    vllm:request_success_total{finished_reason="stop",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 4.0
    # HELP vllm:iteration_tokens_total Histogram of number of tokens per engine_step.
    # TYPE vllm:iteration_tokens_total histogram
    vllm:iteration_tokens_total_sum{model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 1103.0
    vllm:iteration_tokens_total_bucket{le="1.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="2.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="4.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="8.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="16.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="24.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="32.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 994.0
    vllm:iteration_tokens_total_bucket{le="40.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="48.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="56.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="64.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="72.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="80.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="88.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0
    vllm:iteration_tokens_total_bucket{le="96.0",model_name="Qwen/Qwen2.5-Coder-1.5B-Instruct"} 998.0


.. attention::
    The sample above was captured before the ``aibrix:`` names were introduced and shows only
    the pass-through metrics. Current versions also emit the standardized copies listed in the
    table.


Model Downloading
-----------------

The AI Engine Runtime supports downloading models from multiple remote sources, including HuggingFace, S3, and TOS.
This is extremely useful when the control plane needs to interact with the pod to dynamically load new models.


Download From HuggingFace
^^^^^^^^^^^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the HuggingFace model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX="json, safetensors"
    export DOWNLOADER_NUM_THREADS=16
    # HuggingFace settings
    export HF_ENDPOINT=https://hf-mirror.com  # set it when env is in CN region


Then use AI Engine Runtime to download the model from HuggingFace:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri deepseek-ai/deepseek-coder-6.7b-instruct \
        --local-dir /tmp/aibrix/models_hf/


Download From S3
^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the S3 model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX="json, safetensors"
    export DOWNLOADER_NUM_THREADS=16
    # AWS settings
    export AWS_ACCESS_KEY_ID=<INPUT YOUR AWS ACCESS KEY ID>
    export AWS_SECRET_ACCESS_KEY=<INPUT YOUR AWS SECRET ACCESS KEY>
    export AWS_ENDPOINT_URL=<INPUT YOUR AWS ENDPOINT URL> # e.g. https://s3.us-west-2.amazonaws.com
    export AWS_REGION=<INPUT YOUR AWS REGION> # e.g. us-west-2


Then use AI Runtime to download the model from AWS S3:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri s3://aibrix-model-artifacts/deepseek-coder-6.7b-instruct/ \
        --local-dir /tmp/aibrix/models_s3/
    

Download From TOS
^^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the TOS model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX="json, safetensors"
    export DOWNLOADER_NUM_THREADS=16
    # AWS settings
    export TOS_ACCESS_KEY=<INPUT YOUR TOS ACCESS KEY>
    export TOS_SECRET_KEY=<INPUT YOUR TOS SECRET KEY>
    export TOS_ENDPOINT=<INPUT YOUR TOS ENDPOINT> # e.g. https://tos-s3-cn-beijing.volces.com
    export TOS_REGION=<INPUT YOUR TOS REGION> # e..g cn-beijing


Then use AI Runtime to download the model from TOS:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri tos://aibrix-model-artifacts/deepseek-coder-6.7b-instruct/ \
        --local-dir /tmp/aibrix/models_tos/


Model Configuration API
-----------------------

.. attention::
    this needs the engine to starts with `--enable-lora` and env `export VLLM_ALLOW_RUNTIME_LORA_UPDATING=true` enabled.
    You can check `Dynamically serving LoRA Adapters <https://docs.vllm.ai/en/latest/features/lora.html#dynamically-serving-lora-adapters>`_ for more details.


Let's assume you already have a base model and runtime deployed and you want to load a LoRA adapter to it.

.. code-block:: bash

    # start the engine
    VLLM_ALLOW_RUNTIME_LORA_UPDATING=true vllm serve Qwen/Qwen2.5-Coder-1.5B-Instruct --enable-lora
    # start the runtime
    INFERENCE_ENGINE=vllm INFERENCE_ENGINE_ENDPOINT="http://localhost:8000" aibrix_runtime --port 8080


.. code-block:: bash

    curl -X POST http://localhost:8080/v1/lora_adapter/load \
    -H "Content-Type: application/json" \
    -d '{"lora_name": "lora-2", "lora_path": "bharati2324/Qwen2.5-1.5B-Instruct-Code-LoRA-r16v2"}'

.. code-block:: bash

    curl -X POST http://localhost:8080/v1/lora_adapter/unload \
    -H "Content-Type: application/json" \
    -d '{"lora_name": "lora-1"}'

.. code-block:: bash

    curl -X GET  http://localhost:8000/v1/models | jq
    {
        "object": "list",
        "data": [
            {
                "id": "Qwen/Qwen2.5-Coder-1.5B-Instruct",
                "object": "model",
                "created": 1738218097,
                "owned_by": "vllm",
                "root": "Qwen/Qwen2.5-Coder-1.5B-Instruct",
                "parent": null,
                "max_model_len": 32768,
                "permission": [
                    {
                    "id": "modelperm-c2e9860095b745b6b8be7133c5ab1fcf",
                    "object": "model_permission",
                    "created": 1738218097,
                    "allow_create_engine": false,
                    "allow_sampling": true,
                    "allow_logprobs": true,
                    "allow_search_indices": false,
                    "allow_view": true,
                    "allow_fine_tuning": false,
                    "organization": "*",
                    "group": null,
                    "is_blocking": false
                    }
                ]
            },
            {
                "id": "lora-1",
                "object": "model",
                "created": 1738218097,
                "owned_by": "vllm",
                "root": "bharati2324/Qwen2.5-1.5B-Instruct-Code-LoRA-r16v2",
                "parent": "Qwen/Qwen2.5-Coder-1.5B-Instruct",
                "max_model_len": null,
                "permission": [
                    {
                    "id": "modelperm-c21d06b59af0435292c70cd612e68b01",
                    "object": "model_permission",
                    "created": 1738218097,
                    "allow_create_engine": false,
                    "allow_sampling": true,
                    "allow_logprobs": true,
                    "allow_search_indices": false,
                    "allow_view": true,
                    "allow_fine_tuning": false,
                    "organization": "*",
                    "group": null,
                    "is_blocking": false
                    }
                ]
            },
            {
                "id": "lora-2",
                "object": "model",
                "created": 1738218097,
                "owned_by": "vllm",
                "root": "bharati2324/Qwen2.5-1.5B-Instruct-Code-LoRA-r16v2",
                "parent": "Qwen/Qwen2.5-Coder-1.5B-Instruct",
                "max_model_len": null,
                "permission": [
                    {
                    "id": "modelperm-bf2af850171242f7a9f4ccd9ecd313cd",
                    "object": "model_permission",
                    "created": 1738218097,
                    "allow_create_engine": false,
                    "allow_sampling": true,
                    "allow_logprobs": true,
                    "allow_search_indices": false,
                    "allow_view": true,
                    "allow_fine_tuning": false,
                    "organization": "*",
                    "group": null,
                    "is_blocking": false
                    }
                ]
            }
        ]
    }


Configuration reference
-----------------------

**Command line.** ``aibrix_runtime`` accepts ``--host`` (default ``0.0.0.0``), ``--port``
(default ``8080``) and ``--enable-fastapi-docs`` to serve the OpenAPI schema and Swagger UI.

**Environment variables**

.. list-table::
   :header-rows: 1
   :widths: 36 22 42

   * - Variable
     - Default
     - Meaning
   * - ``INFERENCE_ENGINE``
     - ``vllm``
     - Engine type. Must be ``vllm``; the runtime fails to start with any other value.
   * - ``INFERENCE_ENGINE_VERSION``
     - ``0.6.1``
     - Engine version. For vLLM, versions from ``0.6.1`` enable the LoRA endpoints.
   * - ``INFERENCE_ENGINE_ENDPOINT``
     - ``http://localhost:8000``
     - Base URL of the engine.
   * - ``METRIC_SCRAPE_PATH``
     - ``/metrics``
     - Path scraped on the engine.
   * - ``METRICS_ENABLE_TRANSFORMATION``
     - ``1``
     - Apply standardization rules. ``0`` forces raw passthrough.
   * - ``METRICS_RAW_PASSTHROUGH_MODE``
     - ``0``
     - Serve the engine's metrics unchanged.
   * - ``PROMETHEUS_MULTIPROC_DIR``
     - ``/tmp/aibrix/metrics/``
     - Scratch directory for the Prometheus client.
   * - ``DOWNLOADER_LOCAL_DIR``
     - ``/tmp/aibrix/models/``
     - Where models are downloaded when a request does not name a directory.
   * - ``DOWNLOADER_NUM_THREADS``
     - ``32``
     - Parallel download threads.
   * - ``DOWNLOADER_ALLOW_FILE_SUFFIX``
     - *(all files)*
     - Comma-separated suffixes to fetch, for example ``json, safetensors``.
   * - ``DOWNLOADER_PART_THRESHOLD`` / ``DOWNLOADER_PART_CHUNKSIZE``
     - ``67108864``
     - Size in bytes above which a file is fetched in parts, and the part size.
   * - ``DOWNLOADER_FORCE_DOWNLOAD``
     - ``0``
     - Re-download even if the files already exist locally.
   * - ``DOWNLOADER_CHECK_FILE_EXIST``
     - ``1``
     - Skip files that already exist locally.
   * - ``HF_TOKEN``, ``HF_ENDPOINT``, ``HF_REVISION``
     - *(unset)*
     - HuggingFace credentials, mirror endpoint and revision.
   * - ``AWS_ACCESS_KEY_ID``, ``AWS_SECRET_ACCESS_KEY``, ``AWS_ENDPOINT_URL``, ``AWS_REGION``
     - *(unset)*
     - S3 credentials and endpoint. ``DOWNLOADER_S3_MAX_IO_QUEUE`` (``100``) and
       ``DOWNLOADER_S3_IO_CHUNKSIZE`` (``16777216``) tune the transfer.
   * - ``TOS_ACCESS_KEY``, ``TOS_SECRET_KEY``, ``TOS_ENDPOINT``, ``TOS_REGION``
     - *(unset)*
     - TOS credentials and endpoint. ``DOWNLOADER_TOS_VERSION`` (``v2``) selects the client
       API and ``TOS_ENABLE_CRC`` enables checksums.

**Annotations and flags**

.. list-table::
   :header-rows: 1
   :widths: 50 50

   * - Setting
     - Meaning
   * - ``model.aibrix.ai/sidecar-injection: "true"`` (annotation)
     - Ask the webhook to inject the runtime into a ``Deployment`` or ``StormService``.
   * - ``model.aibrix.ai/sidecar-runtime-image`` (annotation)
     - Runtime image to inject instead of the default.
   * - ``--enable-runtime-sidecar`` (controller manager flag, default ``false``)
     - Let controllers use the runtime API when the ``aibrix-runtime`` container is present.

HTTP API reference
------------------

All endpoints are served on the runtime port (``8080`` by default).

.. list-table::
   :header-rows: 1
   :widths: 34 66

   * - Endpoint
     - Description
   * - ``GET /healthz``
     - Liveness. Always ``200`` once the process is up.
   * - ``GET /ready``
     - Readiness. Used as the pod readiness probe when injected.
   * - ``GET /metrics``
     - Standardized engine metrics in Prometheus format.
   * - ``POST /v1/lora_adapter/load``
     - Body ``{"lora_name": ..., "lora_path": ...}``. Loads an adapter on the engine.
       ``lora_path`` is passed to the engine unchanged, so it must be a path the engine can
       open. A second body form, ``{"lora_name": ..., "artifact_url": ...}``, makes the
       runtime download the artifact first; this is what the ModelAdapter controller sends.
   * - ``POST /v1/lora_adapter/unload``
     - Body ``{"lora_name": ...}``.
   * - ``GET /v1/models``
     - Models currently served by the engine, proxied from the engine.
   * - ``POST /v1/model/download``
     - Body ``{"model_uri": ..., "local_dir": ..., "model_name": ..., "download_extra_config": {...}}``.
       Only ``model_uri`` is required.
   * - ``GET /v1/model/list``
     - Lists models present in a local directory. Takes an optional JSON body
       ``{"local_dir": ...}`` (a body, not a query parameter, even though the method is GET);
       without it the runtime's default download directory is listed.
   * - ``/v1/runtime/models/*`` and ``GET /v1/runtime/snapshot``
     - Experimental engine lifecycle endpoints (``activate``, ``deactivate``, ``sleep``,
       ``wake``, ``kv-limit``) used by :doc:`modelclaim`. Not intended for direct use.

.. seealso::

   :doc:`../designs/aibrix-engine-runtime`
       Internal design of the AI Runtime sidecar.
