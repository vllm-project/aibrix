.. _batch_resource_manager:

====================================
Batch Resource Manager for Neoclouds
====================================

The AIBrix batch Resource Manager lets the Console planner lease cloud
GPU capacity before submitting a batch to the metadata service. This is
useful when you want each batch job to run on provider-managed GPU
machines instead of on the Kubernetes cluster where AIBrix is running.

The currently supported cloud providers are:

- ``runpod`` - creates RunPod pods, injects an SSH public key, and runs
  vLLM inside the pod.
- ``lambdaCloud`` - creates Lambda Cloud GPU instances, SSHes to the VM,
  and runs the vLLM serving container with Docker.

.. important::

   Automatic RunPod and Lambda Cloud provisioning is available through the
   Console Job API, ``/api/v1/jobs``. Direct calls to the metadata service
   ``/v1/batches`` do not call Resource Manager by themselves. Direct
   metadata-service users can still pass a runtime manually in
   ``extra_body.aibrix.runtime``, but then they own provisioning and cleanup.


How It Works
------------

::

   User / Console UI
        |
        | POST /api/v1/jobs
        v
   Console JobService
        |
        | enqueue job
        v
   Planner
        |
        | Provision(ResourceProvision)
        v
   Resource Manager
        |
        | provider API
        +--------------------+
        |                    |
        v                    v
   RunPod pod          Lambda Cloud instance
        |                    |
        | status=running     | status=active
        +---------+----------+
                  |
                  | runtime target + SSH options
                  v
   Metadata Service /v1/batches
                  |
                  | SSH-launch runtime starts vLLM and processes JSONL
                  v
              Batch output

The planner lifecycle is:

1. Console receives a job through ``POST /api/v1/jobs``.
2. Planner builds a resource request from the selected
   ``ModelDeploymentTemplate``. Today the Console path requests one replica
   with ``accelerator.count`` GPUs.
3. Resource Manager provisions the cloud resource and stores a
   ``ProvisionResult``.
4. Planner polls Resource Manager until the provision is ``running``.
5. Planner submits the batch to the metadata service with
   ``aibrix.runtime`` set to ``RunPod`` or ``LambdaCloud``.
6. Metadata Service SSHes into the provisioned machine, starts vLLM, waits
   for ``/health``, and dispatches the batch requests.
7. When the batch reaches a terminal state, or when the job is cancelled,
   Planner calls Resource Manager ``Release``. RunPod pods are deleted and
   Lambda Cloud instances are terminated.


Prerequisites
-------------

Before enabling a cloud provider, make sure you have:

- A running AIBrix metadata service with batch file storage configured. See
  :ref:`batch_api`.
- A running AIBrix Console backend. The Resource Manager path is owned by
  the Console planner.
- A private SSH key mounted into the metadata service and exposed through
  ``AIBRIX_BATCH_SSH_KEY_FILE``.
- The matching public key configured for the selected provider:
  ``RUNPOD_SSH_PUBLIC_KEY`` for RunPod, or an SSH key name in the Lambda
  Cloud account for Lambda Cloud.
- A ``ModelDeploymentTemplate`` for the model and GPU shape users can
  select when creating a job.

Generate a dedicated key pair for batch resources:

.. code-block:: bash

   ssh-keygen -t ed25519 -f ~/.ssh/aibrix-batch-rm -C aibrix-batch-rm
   chmod 600 ~/.ssh/aibrix-batch-rm

Mount the private key into the metadata service. For Kubernetes
deployments, create a secret and add a volume/env patch to the metadata
service deployment:

.. code-block:: bash

   kubectl -n aibrix-system create secret generic aibrix-batch-ssh-key \
     --from-file=id_ed25519=$HOME/.ssh/aibrix-batch-rm

.. code-block:: yaml

   spec:
     template:
       spec:
         containers:
         - name: metadata-service
           env:
           - name: AIBRIX_BATCH_SSH_KEY_FILE
             value: /etc/aibrix/batch-ssh/id_ed25519
           volumeMounts:
           - name: batch-ssh-key
             mountPath: /etc/aibrix/batch-ssh
             readOnly: true
         volumes:
         - name: batch-ssh-key
           secret:
             secretName: aibrix-batch-ssh-key
             defaultMode: 0400


Configure the Console Backend
-----------------------------

The Console backend selects exactly one Resource Manager provider at
startup. Use ``PROVISIONER=kubernetes`` for the default in-cluster path,
``PROVISIONER=runpod`` for RunPod, or ``PROVISIONER=lambdaCloud`` for
Lambda Cloud.

.. note::

   Provider identifiers are case-sensitive and intentionally differ by
   context: ``PROVISIONER`` takes the lowercase/camelCase values
   ``runpod`` and ``lambdaCloud``, while the ``aibrix.runtime.target`` field
   in :ref:`batch_model_deployment_templates` uses the PascalCase values
   ``RunPod`` and ``LambdaCloud``.

Common environment variables:

.. list-table::
   :header-rows: 1
   :widths: 30 25 45

   * - Variable
     - Required?
     - Description
   * - ``PROVISIONER``
     - yes
     - ``runpod`` or ``lambdaCloud`` for cloud Resource Manager mode.
   * - ``METADATA_SERVICE_URL``
     - yes
     - Metadata service URL used by Console to submit ``/v1/batches``.
   * - ``STORE_URI``
     - recommended
     - Durable Console store URI. File-backed SQLite is enough for a
       single backend; production should use a shared database.
   * - ``DEFAULT_BATCH_MODEL_DEPLOYMENT_TEMPLATE``
     - optional
     - Template name used when callers omit ``model_template_name``.
   * - ``PLANNING_POLICY``
     - optional
     - Defaults to ``simple``. This is the only policy registered for
       RunPod and Lambda Cloud today.
   * - ``PLANNER_WORKER_COUNT``
     - optional
     - Number of planner workers. Defaults to ``10``.


RunPod
^^^^^^

Required RunPod configuration:

.. code-block:: bash

   export PROVISIONER=runpod
   export RUNPOD_API_KEY=rp_...
   export RUNPOD_SSH_PUBLIC_KEY="$(cat ~/.ssh/aibrix-batch-rm.pub)"

Optional RunPod configuration:

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Variable
     - Description
   * - ``RUNPOD_BASE_URL``
     - Override the RunPod API root. Defaults to
       ``https://rest.runpod.io/v1``.
   * - ``RUNPOD_DATA_CENTERS``
     - Comma-separated RunPod data center IDs. If unset, RunPod can pick
       any available data center.
   * - ``RUNPOD_IMAGE``
     - Pod image. Defaults to ``vllm/vllm-openai:latest``. The image must
       be able to run ``vllm serve`` and allow ``openssh-server`` to be
       installed at pod startup.
   * - ``RUNPOD_CLOUD_TYPE``
     - ``SECURE`` or ``COMMUNITY``. Defaults to ``SECURE``.

RunPod does not expose a full region, GPU type, or pricing catalog through
the REST API used here. AIBrix therefore passes
``ModelDeploymentTemplate.spec.accelerator.type`` directly as the RunPod
``gpuTypeIds`` value. Use the exact GPU type string accepted by RunPod,
for example ``NVIDIA H100 80GB HBM3``.

RunPod networking:

- Resource Manager creates one pod per requested replica.
- The pod starts ``sshd`` and injects ``RUNPOD_SSH_PUBLIC_KEY``.
- Planner passes ``root``, the public IP, SSH port, and
  ``https://<pod-id>-8000.proxy.runpod.net`` to the metadata service.
- The RunPod runtime launches ``vllm serve`` over SSH and dispatches
  requests through the RunPod HTTP proxy.


Lambda Cloud
^^^^^^^^^^^^

First add the public key to your Lambda Cloud account and note its key
name. Then configure the Console backend:

.. code-block:: bash

   export PROVISIONER=lambdaCloud
   export LAMBDA_CLOUD_API_KEY=...
   export LAMBDA_CLOUD_SSH_KEYS=aibrix-batch-rm

Optional Lambda Cloud configuration:

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Variable
     - Description
   * - ``LAMBDA_CLOUD_BASE_URL``
     - Override the Lambda Cloud API root. Defaults to
       ``https://cloud.lambda.ai/api/v1``.
   * - ``LAMBDA_CLOUD_REGION``
     - Hard default region preference, such as ``us-west-1``. If unset,
       AIBrix picks the cheapest matching instance type with current
       capacity in any available region.
   * - ``LAMBDA_CLOUD_SSH_KEYS``
     - Comma-separated SSH key names already registered in Lambda Cloud.
       At least one key is required.

Lambda Cloud capacity selection:

- ``accelerator.count`` becomes the required GPU count per instance.
- ``accelerator.type`` is matched against Lambda Cloud instance type names
  and GPU descriptions. Names such as ``H100``, ``NVIDIA H100``, and
  ``H100-SXM5`` normalize to the same GPU family.
- If ``LAMBDA_CLOUD_REGION`` is set, AIBrix only accepts capacity in that
  region. Otherwise it chooses the cheapest matching instance type among
  regions with capacity.

Lambda Cloud networking:

- Resource Manager launches a GPU VM and waits until it is ``active``.
- Planner passes ``ubuntu``, the public IP, SSH port ``22``, and the model
  template serving image to the metadata service.
- The Lambda Cloud runtime SSHes to the VM, verifies Docker, and runs the
  vLLM container with ``sudo docker run --gpus all``.
- vLLM binds to ``127.0.0.1:8000`` on the VM. The metadata service opens an
  SSH local port-forward and dispatches through the tunnel, so the vLLM
  endpoint is not exposed publicly.


Create a Model Deployment Template
----------------------------------

The cloud Resource Manager path uses the Console
``ModelDeploymentTemplate`` selected by the batch job. The template remains
provider-agnostic; the active provider is selected by ``PROVISIONER``.

For a new model, create a Console model first:

.. code-block:: bash

   export CONSOLE=http://localhost:8080

   MODEL_ID=$(curl -sS -X POST "$CONSOLE/api/v1/models" \
     -H "Content-Type: application/json" \
     -d '{
       "name": "Qwen2.5 7B Instruct",
       "categories": ["LLM"],
       "serving_name": "Qwen/Qwen2.5-7B-Instruct"
     }' | jq -r .id)

Then create a deployment template:

.. code-block:: bash

   curl -sS -X POST "$CONSOLE/api/v1/models/${MODEL_ID}/deployment-templates" \
     -H "Content-Type: application/json" \
     -d '{
       "name": "qwen2-5-7b-cloud",
       "version": "v1.0.0",
       "status": "active",
       "spec": {
         "engine": {
           "type": "vllm",
           "version": "latest",
           "image": "vllm/vllm-openai:latest",
           "invocation": "http_server",
           "serve_args": ["--trust-remote-code"],
           "health_endpoint": "/health",
           "ready_timeout_seconds": 1800,
           "metrics_endpoint": "/metrics"
         },
         "model_source": {
           "type": "huggingface",
           "uri": "Qwen/Qwen2.5-7B-Instruct"
         },
         "accelerator": {
           "type": "H100",
           "count": 1,
           "vram_gb": 80,
           "interconnect": "pcie"
         },
         "parallelism": {
           "tp": 1,
           "pp": 1,
           "dp": 1,
           "ep": 1
         },
         "engine_args": {
           "gpu_memory_utilization": "0.90"
         },
         "quantization": {
           "weight": "fp16",
           "kv_cache": "auto"
         },
         "supported_endpoints": ["/v1/chat/completions"],
         "deployment_mode": "dedicated"
       }
     }'

Provider-specific notes:

- For Lambda Cloud, ``accelerator.type`` should be a GPU family or Lambda
  instance-type token, such as ``A10``, ``A100``, or ``H100``.
- For RunPod, set ``accelerator.type`` to the exact RunPod GPU type ID
  accepted by the pod API, such as ``NVIDIA H100 80GB HBM3``.
- For Lambda Cloud, ``engine.image`` is the Docker image run on the VM.
- For RunPod, ``RUNPOD_IMAGE`` controls the pod image. The runtime still
  uses template fields such as ``model_source.uri`` and ``engine.serve_args``
  when launching ``vllm serve``.


Submit a Batch Job
------------------

Prepare a JSONL input file:

.. code-block:: json

   {"custom_id":"request-1","method":"POST","url":"/v1/chat/completions","body":{"model":"Qwen/Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"Explain AIBrix in one paragraph."}],"max_tokens":128}}
   {"custom_id":"request-2","method":"POST","url":"/v1/chat/completions","body":{"model":"Qwen/Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"What is batch inference?"}],"max_tokens":128}}

Upload it through the Console file proxy:

.. code-block:: bash

   FILE_ID=$(curl -sS -X POST "$CONSOLE/api/v1/files/upload" \
     -F "purpose=batch" \
     -F "file=@batch_input.jsonl" | jq -r .id)

Create a job:

.. code-block:: bash

   JOB_ID=$(curl -sS -X POST "$CONSOLE/api/v1/jobs" \
     -H "Content-Type: application/json" \
     -d "{
       \"input_dataset\": \"${FILE_ID}\",
       \"endpoint\": \"/v1/chat/completions\",
       \"completion_window\": \"24h\",
       \"name\": \"qwen-cloud-batch\",
       \"model_id\": \"${MODEL_ID}\",
       \"model_template_name\": \"qwen2-5-7b-cloud\",
       \"model_template_version\": \"v1.0.0\"
     }" | jq -r .id)

Watch the job and provision state:

.. code-block:: bash

   watch -n 5 \
     "curl -sS $CONSOLE/api/v1/jobs/${JOB_ID} | jq '{id,status,batch_id,provision_id,provision,events}'"

Expected high-level state flow:

::

   queued -> resource_preparing -> submitting -> batch_created -> in_progress -> finalizing -> completed

If resource provisioning fails, the job moves to ``resource_failed`` and
``error_message`` contains the provider error. If metadata-service
submission fails after resources are ready, the job moves to
``submit_failed`` and Planner attempts to release the provision.

Cancel a running job:

.. code-block:: bash

   curl -sS -X POST "$CONSOLE/api/v1/jobs/${JOB_ID}/cancel" \
     -H "Content-Type: application/json" \
     -d '{}'

Planner forwards cancellation to the metadata service if a batch has been
submitted, then releases the cloud provision.


Read Results
------------

When the job reaches ``completed``, download the output file through the
Console file proxy:

.. code-block:: bash

   OUTPUT_FILE_ID=$(curl -sS "$CONSOLE/api/v1/jobs/${JOB_ID}" | jq -r .output_dataset)

   curl -sS "$CONSOLE/api/v1/files/${OUTPUT_FILE_ID}/content" \
     -o batch_output.jsonl

   jq . batch_output.jsonl


Troubleshooting
---------------

``resource manager init: missing credential``
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The selected provider was enabled without its required environment variables.
Check:

- RunPod: ``RUNPOD_API_KEY`` and ``RUNPOD_SSH_PUBLIC_KEY``.
- Lambda Cloud: ``LAMBDA_CLOUD_API_KEY`` and ``LAMBDA_CLOUD_SSH_KEYS``.

``resource_failed`` with ``NoGpuType`` on RunPod
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

RunPod requires at least one GPU type. Set
``ModelDeploymentTemplate.spec.accelerator.type`` to an exact RunPod GPU type
ID accepted by the pod API.

``resource_failed`` with ``NoCapacity`` on Lambda Cloud
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Lambda Cloud had no currently available instance type matching
``accelerator.type``, ``accelerator.count``, and ``LAMBDA_CLOUD_REGION``.
Try a different GPU family, lower GPU count, or unset/change the region.

Job is stuck in ``resource_preparing``
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The provider accepted the launch but AIBrix has not observed a ready public
IP/SSH endpoint yet. Check the provider console, then inspect Console logs
for provider API errors. For RunPod, the provision becomes ready only when
the pod is ``RUNNING`` and has a public IP. For Lambda Cloud, it becomes
ready when the instance is ``active`` and has a public IP.

Job reaches ``submit_failed`` or vLLM never becomes healthy
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The resource was created, but the metadata service could not launch or
reach vLLM. Check:

- ``AIBRIX_BATCH_SSH_KEY_FILE`` points to the private key matching the
  provider-side public key.
- The private key file is readable only by the metadata-service process.
- Lambda Cloud instances allow SSH for the registered key name.
- RunPod pod image can install and run ``openssh-server`` and has ``vllm``
  on ``PATH``.
- The model can be downloaded from ``model_source.uri``. If it requires
  authentication, configure the model source secret before submitting jobs.
- Lambda Cloud images can be started with Docker and the NVIDIA runtime.

Provider resources remain after a failed job
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Planner releases resources on terminal states and cancellation, but cleanup
is best-effort. If Console or the provider API fails during cleanup, use the
``provision.raw_json`` field from ``GET /api/v1/jobs/{id}`` to find the
RunPod pod ID or Lambda Cloud instance IDs, then delete them in the provider
console.


Current Limitations
-------------------

- Cloud Resource Manager mode is selected once per Console backend with
  ``PROVISIONER``. Per-job provider selection is not exposed yet.
- The Console planner path requests one replica per batch job today.
  ``ResourceGroupSpec`` supports richer groups internally, but the Console
  job path currently derives only one group from the selected template.
- RunPod catalog, pricing, and region discovery return empty results because
  the RunPod REST API used by AIBrix does not expose those catalogs.
- Lambda Cloud catalog data is fetched live from the Lambda Cloud API, but
  there is no public Console catalog endpoint for it yet.
- The cloud runtime path is designed for vLLM-compatible OpenAI HTTP
  serving over SSH. Other engines need matching runtime support before they
  can be used with RunPod or Lambda Cloud.


See Also
--------

- :ref:`batch_api` - OpenAI-compatible batch API and file workflow.
- :ref:`batch_model_deployment_templates` - model deployment templates.
- ``apps/console/api/resource_manager`` - Resource Manager provider
  implementations.
- ``python/aibrix/aibrix/batch/job_driver/runtime`` - metadata-service
  runtime implementations for Kubernetes, RunPod, and Lambda Cloud.
