.. _batch_model_deployment_templates:

================================
Batch Model Deployment Templates
================================

AIBrix Batch carries model deployment intent through
``extra_body.aibrix`` on the OpenAI-compatible Batch API. The current
runtime contract is:

- ``aibrix.model_template`` describes the engine, model source, accelerator,
  parallelism, and supported endpoints.
- ``aibrix.runtime`` selects the MDS runtime backend, such as
  ``Kubernetes``, ``KubernetesJob``, ``LambdaCloud``, ``RunPod``, or
  ``External``.
- ``aibrix.resource_allocation`` records resource-manager output, such as
  the provision id returned by Console's Resource Manager.


Why Templates Exist
-------------------

OpenAI's Batch API treats the model as a black-box string, for example
``model: "gpt-4-turbo"``. Self-hosted deployments need more control:

- inference engine and image, such as vLLM or SGLang
- GPU Type and GPU count
- model artifact location
- tensor, pipeline, data, and expert parallelism
- engine flags such as max model length or prefix caching
- supported OpenAI endpoints for the selected model

Templates keep those controls in platform-owned metadata while preserving
the OpenAI SDK request shape.


Current Data Flow
-----------------

::

   Console user
      |
      | CreateJob(model_template_name, model_template_version, model_id)
      v
   Console backend
      |
      | resolves ModelDeploymentTemplate from Console store
      | marshals template.spec as snake_case JSON
      | planner reads accelerator + engine serving fields
      v
   Resource Manager / Planner
      |
      | POST /v1/batches with extra_body.aibrix:
      |   job_id
      |   model
      |   runtime
      |   resource_allocation
      |   model_template { name, version, spec }
      v
   Metadata Service
      |
      | validates runtime target and persists AIBrix metadata
      | selected runtime consumes the inline template spec when needed
      v
   Runtime backend
      |
      +-- Kubernetes / KubernetesJob: render Kubernetes manifests
      +-- LambdaCloud / RunPod: SSH-launch vLLM on provisioned compute
      +-- External: dispatch to an already known endpoint

Direct MDS callers can also submit ``extra_body.aibrix`` themselves. If the
MDS deployment has no template registry configured, direct callers must
include ``aibrix.model_template.spec`` inline.

Quick Start: Console Path
-------------------------

The Console path is the supported path for Resource Manager backed batch
jobs, including RunPod and Lambda Cloud.

1. Register or create a model deployment template in the Console store.
   The template must include at least ``engine``, ``model_source``,
   ``accelerator``, and ``supported_endpoints``.

2. Submit a Console job with the selected template:

   .. code-block:: json

      {
        "name": "daily-eval",
        "input_dataset": "file-abc",
        "endpoint": "/v1/chat/completions",
        "completion_window": "24h",
        "model_id": "model-123",
        "model_template_name": "llama3-8b-a10",
        "model_template_version": "v1"
      }

3. Console resolves the template, sends ``aibrix.model_template.spec`` to
   MDS, and the planner uses the same spec to provision resources.

For cloud providers, the Console backend must also be configured with a
single Resource Manager provider via ``PROVISIONER``. See
:doc:`batch-resource-manager`.


Direct MDS Request Shape
------------------------

Direct callers use the OpenAI SDK's ``extra_body`` escape hatch. This
example uses an inline template spec so it does not depend on a ConfigMap
registry:

.. code-block:: python

   from openai import OpenAI

   client = OpenAI(base_url="http://aibrix-metadata.example/v1", api_key="...")

   batch = client.batches.create(
       input_file_id="file-abc",
       endpoint="/v1/chat/completions",
       completion_window="24h",
       metadata={"team": "ml-platform"},
       extra_body={
           "aibrix": {
               "model": "meta-llama/Llama-3.1-8B-Instruct",
               "runtime": {
                   "target": "External",
                   "options": {},
               },
               "model_template": {
                   "name": "llama3-8b-a10",
                   "version": "v1",
                   "spec": {
                       "engine": {
                           "type": "vllm",
                           "version": "0.6.3",
                           "image": "vllm/vllm-openai:latest",
                           "serve_args": ["--gpu-memory-utilization", "0.90"],
                       },
                       "model_source": {
                           "type": "huggingface",
                           "uri": "meta-llama/Llama-3.1-8B-Instruct",
                       },
                       "accelerator": {
                           "type": "A10",
                           "count": 1,
                       },
                       "parallelism": {
                           "tp": 1,
                           "pp": 1,
                           "dp": 1,
                           "ep": 1,
                       },
                       "supported_endpoints": ["/v1/chat/completions"],
                       "deployment_mode": "dedicated",
                   },
               },
           },
       },
   )

The response exposes the persisted AIBrix block in ``batch.aibrix``. There
is no current ``_aibrix.resolved_endpoint`` response field.


ModelDeploymentTemplate Schema
------------------------------

The source of truth is :py:mod:`aibrix.batch.template.schema`.
``ModelDeploymentTemplateSpec`` is strict: unknown fields are rejected. In
particular, current templates do not have a ``provider_config`` field.
Provider selection belongs to Resource Manager / runtime selection, not to
the template schema.

**Required top-level fields when a complete template object is stored:**

- ``name``: logical template name.
- ``version``: version string. If omitted in an inline spec path, consumers
  use the ref version or schema default.
- ``status``: ``active``, ``deprecated``, or ``draft`` for registry-backed
  templates.
- ``spec``: the deployment body.

**Required fields inside ``spec``:**

.. list-table::
   :header-rows: 1
   :widths: 22 28 50

   * - Field
     - Required?
     - Current meaning
   * - ``engine``
     - yes
     - Engine type, version, image, raw ``serve_args``, health endpoint, and
       readiness timeout. ``vllm`` and ``mock`` have manifest adapters today.
   * - ``model_source``
     - yes
     - Model artifact source. Kubernetes renderers can use this for model
       download and vLLM ``--model`` args. Cloud SSH runtimes require the
       resolved ``aibrix.model`` to be directly loadable by vLLM.
   * - ``accelerator``
     - yes
     - Free-form GPU type and GPU count. Console Resource Manager reads
       ``type`` and ``count`` to request cloud resources.
   * - ``supported_endpoints``
     - yes
     - OpenAI endpoints this deployment can serve, such as
       ``/v1/chat/completions``.

**Optional/defaulted fields inside ``spec``:**

.. list-table::
   :header-rows: 1
   :widths: 22 28 50

   * - Field
     - Default
     - Current meaning
   * - ``parallelism``
     - all degrees ``1``
     - ``tp * pp * dp * ep`` must equal ``accelerator.count``.
   * - ``engine_args``
     - empty
     - Typed and extra engine flags. Kubernetes vLLM rendering converts these
       into CLI flags. Console cloud runtime construction does not pass this
       field today; use ``engine.serve_args`` for LambdaCloud/RunPod flags.
   * - ``quantization``
     - empty
     - Kubernetes vLLM rendering maps weight and KV-cache quantization into
       CLI flags. Cloud SSH runtimes honor only flags that appear in
       ``engine.serve_args`` today.
   * - ``service_id``
     - ``null``
     - Optional service/discovery identifier for Kubernetes manifest labels
       and discovery paths.
   * - ``deployment_mode``
     - ``dedicated``
     - ``dedicated`` is the only fully honored mode today. ``shared`` and
       ``external`` are schema values but are not accepted by the Kubernetes
       Job renderer.


Runtime Consumption Matrix
--------------------------

.. list-table::
   :header-rows: 1
   :widths: 22 39 39

   * - Field
     - Kubernetes renderers
     - Console cloud path
   * - ``engine.image``
     - Engine container image.
     - Passed to MDS runtime options. LambdaCloud uses it as the Docker image.
       RunPod provisioning uses ``RUNPOD_IMAGE`` for the pod image today.
   * - ``engine.serve_args``
     - Appended last to engine CLI args, so admin raw flags can override
       generated args.
     - Passed as runtime ``vllm_args`` for LambdaCloud and RunPod.
   * - ``engine_args``
     - Converted to vLLM CLI flags by the engine adapter.
     - Not consumed by Console's cloud runtime construction today.
   * - ``model_source``
     - Used by downloader/model args and auth secret rendering.
     - Used by Console to resolve ``aibrix.model`` when no model serving name
       is set. Cloud SSH runtimes do not apply Kubernetes secrets.
   * - ``accelerator``
     - Used for resource requests and validation.
     - Used by Resource Manager scheduling. RunPod requires provider-accepted
       GPU type strings; LambdaCloud normalizes common GPU family names.
   * - ``parallelism`` and ``quantization``
     - Used by Kubernetes vLLM argument rendering.
     - Not read directly; express required cloud flags in ``serve_args``.


``extra_body.aibrix`` Contract
------------------------------

The MDS API accepts this AIBrix extension block:

.. code-block:: json

   {
     "aibrix": {
       "job_id": "job_123",
       "model": "meta-llama/Llama-3.1-8B-Instruct",
       "runtime": {
         "target": "RunPod",
         "options": {
           "host": "1.2.3.4",
           "ssh_port": 22,
           "ssh_user": "root",
           "http_base_url": "https://pod-8000.proxy.runpod.net",
           "model": "meta-llama/Llama-3.1-8B-Instruct",
           "vllm_args": ["--gpu-memory-utilization", "0.90"]
         }
       },
       "resource_allocation": {
         "provision_id": "runpod-abc"
       },
       "model_template": {
         "name": "llama3-8b-a10",
         "version": "v1",
         "spec": {}
       },
       "client": {
         "max_concurrency": 256,
         "adaptive_concurrency": true,
         "adaptive_max_factor": 16,
         "retry_policy": {
           "max_retries": 5,
           "base_delay_seconds": 2,
           "max_delay_seconds": 10,
           "no_endpoint_max_retries": 5
         }
       }
     }
   }

``client`` controls per-job smart-client behavior. ``max_concurrency`` is an
absolute job-global in-flight cap. If adaptive concurrency is enabled, the cap
limits adaptive growth; if adaptive concurrency is disabled, it becomes the
fixed concurrency. Omitted fields fall back to metadata-service environment
defaults and then built-in defaults. This public block intentionally does not
expose telemetry interval, QPS, request timeout, or fine-grained adaptive
controller internals.

The known upstream runtime targets are:

- ``Kubernetes``
- ``KubernetesJob``
- ``LambdaCloud``
- ``RunPod``
- ``External``

``runtime.target`` is validated against the live runtime registry, so
downstream runtimes can register additional target strings.


Operational Notes
-----------------

- For Resource Manager backed jobs, submit through Console. Direct MDS
  ``/v1/batches`` requests do not provision RunPod or Lambda Cloud resources.
- Console currently selects one Resource Manager provider per backend process
  via ``PROVISIONER``. Per-job provider preference is not implemented.
- Cloud SSH runtimes require ``aibrix.model`` to be directly loadable by
  vLLM on the remote host. Template ``auth_secret_ref`` is a Kubernetes
  manifest feature and is not automatically applied to LambdaCloud/RunPod.
- Use ``AIBRIX_MDS_HTTP_BODY_LOG=1`` on MDS when debugging the exact
  ``extra_body.aibrix`` payload received by the metadata service.


See Also
--------

- :doc:`batch-api` - OpenAI Batch API surface and request/response details
- :doc:`batch-resource-manager` - using RunPod and Lambda Cloud with the
  Console batch planner
- :py:mod:`aibrix.batch.template.schema` - Pydantic source of truth
