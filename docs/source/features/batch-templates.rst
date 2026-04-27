.. _batch_templates:

=============================================
Batch Model Deployment Templates and Profiles
=============================================

The AIBrix Batch service supports white-box deployment configuration via
two ConfigMap-driven resource types:

- **ModelDeploymentTemplate** describes *what* engine and hardware to
  use for a given model: image, GPU SKU, parallelism, engine tuning,
  quantization, model source.
- **BatchProfile** describes *how* a batch should be scheduled and
  stored: storage backend, completion window, per-batch limits.

End users reference these by name through the OpenAI Batch API's
``extra_body`` field, keeping wire compatibility with the official
``openai`` SDK while letting platform admins white-box every aspect
of execution.


Why
---

OpenAI's Batch API treats the model and its execution as a black box:
``model: "gpt-4-turbo"`` is the only handle. For self-hosted
deployments, operators need to control:

- Which inference engine (vLLM, SGLang, TensorRT-LLM, ...)
- GPU SKU and count (H100x4, A100x2, L40Sx1, ...)
- Tensor / pipeline / data parallelism
- Engine tuning (max_num_batched_tokens, prefix caching, quantization)
- Where model weights live (HuggingFace, S3, internal registry)
- Storage backend for input/output files
- Per-profile batch size limits

All of these are admin-time decisions. End users keep the OpenAI ergonomics:
they reference a pre-registered template name, optionally override a
small allowlisted set of fields per batch.


Architecture
------------

::

   Admin                     End User
     |                          |
     | kubectl apply -f         | client.batches.create(
     |    templates.yaml        |     extra_body={"aibrix": {
     |    profiles.yaml         |       "model_template": {"name": "llama3-70b-prod"},
     |                          |       "profile":        {"name": "prod-24h"},
     v                          |     }})
   ConfigMaps                   |
     (aibrix-system)            v
     |                       Metadata Service
     |                          |
     |  watched by              | resolves template + profile + overrides
     v                          v
   TemplateRegistry  -->  Manifest Renderer  -->  K8s Job (vLLM pod, etc.)
   ProfileRegistry

ConfigMap names are fixed:

- ``aibrix-model-deployment-templates`` in namespace ``aibrix-system``
- ``aibrix-batch-profiles`` in namespace ``aibrix-system``

The metadata service watches both ConfigMaps and reloads on change;
no service restart is required for schema-valid edits.


Quick Start
-----------

1. Apply the sample ConfigMaps:

   .. code-block:: bash

      kubectl apply -f config/samples/batch_v1alpha1_model_deployment_templates.yaml
      kubectl apply -f config/samples/batch_v1alpha1_batch_profiles.yaml

2. Submit a batch referencing a template:

   .. code-block:: python

      from openai import OpenAI

      client = OpenAI(base_url="http://aibrix-batch.example/v1", api_key="...")

      batch = client.batches.create(
          input_file_id="file-abc",
          endpoint="/v1/chat/completions",
          completion_window="24h",
          metadata={"team": "ml-platform"},
          extra_body={
              "aibrix": {
                  "model_template": {
                      "name": "llama3-70b-prod",
                      # "version": "v1.3.0",  # optional; "" / omit = latest active
                  },
                  # 'profile' is optional; default profile applies if omitted
              }
          },
      )

3. Check the resolved configuration in the batch object's
   ``_aibrix.resolved_endpoint`` field (visible to admins via the
   metadata service ``/v1/batches/{id}`` endpoint).


ModelDeploymentTemplate Schema
------------------------------

Each item in the templates ConfigMap is a versioned, named entry.
The full schema is defined in :py:mod:`aibrix.batch.template.schema`.

**Required top-level fields:**

- ``name`` (string): logical name. End users reference this.
- ``version`` (string): SemVer-ish; multiple versions can coexist.
- ``status``: ``active``, ``deprecated``, or ``draft``. The registry
  selects the unique active version per name.
- ``spec``: the deployment body, see below.

**spec subfields:**

.. list-table::
   :header-rows: 1
   :widths: 20 30 50

   * - Field
     - Required?
     - Description
   * - ``engine``
     - yes
     - ``type`` (vllm / sglang / trtllm / lmdeploy / mock),
       ``version``, ``image``, ``serve_args``, ``health_endpoint``
   * - ``model_source``
     - yes
     - ``type`` (huggingface / s3 / local / registry), ``uri``,
       optional ``tokenizer_path``, ``chat_template_path``,
       ``auth_secret_ref``
   * - ``accelerator``
     - yes
     - ``type`` (free-form GPU SKU), ``count``, optional ``vram_gb``,
       ``interconnect`` (nvlink / pcie / ib)
   * - ``parallelism``
     - default
     - ``tp``, ``pp``, ``dp``, ``ep`` (default 1). Cross-field rule:
       ``tp * pp * dp * ep`` must equal ``accelerator.count``.
   * - ``engine_args``
     - default
     - Tuning knobs: ``max_num_batched_tokens``, ``max_num_seqs``,
       ``max_model_len``, ``gpu_memory_utilization``,
       ``enable_prefix_caching``, etc. Engine-specific extras are
       passed through transparently.
   * - ``quantization``
     - default
     - ``weight`` (fp8 / awq / gptq / int8 / bf16 / fp16),
       ``kv_cache`` (auto / fp8 / fp8_e4m3 / fp8_e5m2 / int8)
   * - ``provider_config``
     - yes
     - ``type`` (k8s / runpod / lambda_labs / ec2 / gcp / external)
       plus provider-specific fields. Today only ``k8s`` is honored.
   * - ``supported_endpoints``
     - yes
     - List of OpenAI endpoints this model can serve. Validated
       against batch ``endpoint`` at submission time.
   * - ``deployment_mode``
     - default
     - ``dedicated`` (currently the only honored value), ``shared``,
       ``external``


BatchProfile Schema
-------------------

Profiles are simpler: storage settings, scheduling policy, per-batch
quotas. The full schema is in :py:mod:`aibrix.batch.template.schema`.

.. list-table::
   :header-rows: 1
   :widths: 20 30 50

   * - Field
     - Required?
     - Description
   * - ``storage``
     - yes
     - ``backend`` (s3 / minio / gcs / tos / local), ``bucket``,
       optional ``region``, ``credentials_secret_ref``,
       ``endpoint_url``
   * - ``scheduling``
     - default
     - ``completion_window`` (1h / 4h / 24h / best_effort,
       only 24h is honored at runtime today), ``priority``,
       ``max_concurrency``, ``request_timeout_seconds``
   * - ``quota``
     - default
     - ``max_requests_per_batch`` (default 50000),
       ``max_input_size_mb`` (default 200), ``max_output_size_gb``
       (default 10). Enforced at validating phase.
   * - ``openai_service_tier_alias``
     - optional
     - Maps OpenAI's ``service_tier`` field to this profile.


Honored vs Deferred Fields
--------------------------

The schema accepts all design fields for forward compatibility, but
the implementation honors only a subset today. Fields with non-default
deferred values trigger a warning at registry load time so admins
know which configuration is not yet active.

**Honored today**

- All ``engine`` fields (image, version, serve_args, health, timeout)
- All ``model_source`` fields
- ``accelerator.type``, ``accelerator.count``
- ``parallelism`` (tp/pp/dp/ep, with cross-field validation)
- All ``engine_args`` fields including engine-specific extras
- ``quantization`` (weight + kv_cache)
- ``provider_config`` for ``type: k8s`` only
- ``supported_endpoints``
- ``deployment_mode: dedicated`` only
- ``storage`` (all fields)
- ``scheduling.completion_window``: only ``24h`` honored
- ``scheduling.max_concurrency``, ``scheduling.request_timeout_seconds``
- ``quota`` (all fields, enforced at batch validating)

**Accepted but not yet honored**

- ``deployment_mode: shared`` and ``external`` (Phase 2; client/server split)
- ``provider_config`` types other than ``k8s`` (Phase 3 multi-cloud)
- ``scheduling.completion_window`` values other than ``24h`` (Phase 4)
- ``scheduling.priority`` (Phase 4)
- ``scheduling.provider_preference`` (Phase 3)
- ``scheduling.allow_preempt``, ``allow_spot`` (Phase 4)
- ``scheduling.retry_policy`` (Phase 2 smart client)
- ``openai_service_tier_alias`` (Phase 4)


User-Facing Fields in ``extra_body.aibrix``
-------------------------------------------

End users select templates and profiles via OpenAI SDK's
``extra_body`` mechanism. Each reference is a nested object with its
own ``overrides`` namespace; this layout is the authoritative contract
documented in ``apps/console/api/proto/console/v1/console.proto``.

.. code-block:: python

   extra_body = {
       "aibrix": {
           "model_template": {
               "name":    "llama3-70b-prod",   # required
               "version": "v1.3.0",            # optional; "" / omit = latest active
               "overrides": {                  # optional, allowlisted
                   "engine_args": {"max_num_seqs": 512},
               },
           },
           "profile": {
               "name": "prod-24h",             # required when block present
               "overrides": {                  # optional, allowlisted
                   "scheduling": {"max_concurrency": 32},
               },
           },
       }
   }

**Override allowlists** (unknown keys are rejected with 400, never
silently dropped):

- ``model_template.overrides.engine_args``: any field present in
  ``EngineArgsSpec``. Merged into ``template.spec.engine_args`` at render
  time.
- ``profile.overrides.scheduling``: a subset of ``SchedulingSpec``
  fields. Roundtripped via annotations; the deadline-aware scheduler
  consumes it once that work lands.

Sensitive fields (``image``, ``accelerator.type``, ``provider_config``,
``model_source``) are **not** user-overridable. Administrators must
update the ConfigMap to change them.

Inline ``model_template_spec`` is intentionally not supported.
Templates are the curated security/cost gate; allowing inline would
let users bypass image / GPU SKU / namespace controls and would shatter
audit and cost-attribution by template name.


Admin Workflow
--------------

**Adding a new template:**

1. Edit the ConfigMap directly:

   .. code-block:: bash

      kubectl edit configmap -n aibrix-system aibrix-model-deployment-templates

   Or maintain the YAML in Git and apply:

   .. code-block:: bash

      kubectl apply -f my-templates.yaml

2. The metadata service detects the change and reloads. Watch logs for
   load errors:

   .. code-block:: bash

      kubectl logs -n aibrix-system deployment/aibrix-metadata --tail=50 | grep -i template

3. New batches submitted with ``extra_body.aibrix.model_template``
   referencing the new entry pick up the change immediately.
   Existing in-flight batches continue with the version baked into
   their K8s Job at creation time (immutable Job spec).

**Deprecating a version:**

Set ``status: deprecated`` on the older version. The registry rejects
new submissions for deprecated versions but lets in-flight batches
finish.

**Removing a profile:**

Delete the profile entry from the ConfigMap. Subsequent batches
referencing it will fail at validating phase. Active batches are
unaffected.


Standalone (non-Kubernetes) Deployments
---------------------------------------

For non-Kubernetes deployments (local development, testing, or
single-node open-source use), the metadata service falls back to
file-based template loading. Place template and profile YAML files
under a configured directory; the schema is identical to the
ConfigMap ``data.templates.yaml`` and ``data.profiles.yaml`` blocks.


See Also
--------

- :doc:`batch-api` — OpenAI Batch API surface and request/response details
- ``config/samples/batch_v1alpha1_model_deployment_templates.yaml`` — annotated example
- ``config/samples/batch_v1alpha1_batch_profiles.yaml`` — annotated example
- :py:mod:`aibrix.batch.template.schema` — Pydantic source of truth
