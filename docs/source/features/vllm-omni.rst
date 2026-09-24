.. _vllm-omni-serving:

===================================
vLLM-Omni Multi-Modal Serving
===================================

`vLLM-Omni <https://docs.vllm.ai/projects/vllm-omni/en/stable/>`_ extends vLLM's OpenAI-compatible serving to non-text modalities — image generation/editing, speech and general audio, and video generation — behind the same ``vllm serve`` entrypoint you already use for LLMs. AIBrix's gateway understands these endpoints natively: it routes requests to the right backend pods and, for video generation specifically, keeps track of which pod owns each in-progress job so follow-up calls land back on it.

This guide walks through deploying a vLLM-Omni video generation model — `Wan-AI/Wan2.1-VACE-1.3B-diffusers <https://huggingface.co/Wan-AI/Wan2.1-VACE-1.3B-diffusers>`_ — and driving it through AIBrix's gateway, both synchronously and as an async job you poll and download.

.. note::
    The sample manifest referenced in this guide lives at `samples/vllm_omni/wan2.1-vace-1.3b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/wan2.1-vace-1.3b.yaml>`_ in the AIBrix repository. A text+audio example (`Qwen/Qwen2.5-Omni-7B <https://huggingface.co/Qwen/Qwen2.5-Omni-7B>`_) is also available at `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_.

----

How It Works
------------

A vLLM-Omni pod is started the same way as any other model in AIBrix — a ``Deployment`` labeled with ``model.aibrix.ai/name`` and a matching ``Service`` — with one difference: passing ``--omni`` to ``vllm serve``. That flag auto-detects the model's task from its architecture and exposes the matching OpenAI-compatible endpoint (``/v1/images/generations``, ``/v1/audio/speech``, ``/v1/videos``, ...).

The stock gateway already matches ``/v1/videos`` on its own reserved HTTPRoutes (so requests reach ext_proc before a ``model`` header exists) and uses 600s Envoy / ext_proc / model-HTTPRoute timeouts so synchronous generation can finish. The Videos API gets routes of its own — ``aibrix-reserved-router-videos`` for the JSON endpoints and ``aibrix-reserved-router-videos-streaming`` for ``/v1/videos/sync`` and ``/v1/videos/{id}/content`` — because those two groups need opposite ext_proc response-body modes; see the troubleshooting note on truncated responses below. If you installed an older AIBrix release, apply both routes with their ``EnvoyExtensionPolicy`` objects (``config/gateway/gateway-plugin``) and set ``AIBRIX_GATEWAY_TIMEOUT_SECONDS=600`` on the controller-manager.

Video generation needs a bit more from the gateway than a stateless chat completion does. Because a generated video is written to local disk on whichever pod created it, the gateway has to remember which pod owns each job and route follow-up calls back to that exact pod. It does so through an asynchronous-job registry: on a successful create it mints its own opaque **public job ID** and hands that to the client instead of the engine's ID, keeping the backend ID and the owning pod's identity to itself.

.. code-block:: text

    POST /v1/videos              ──►  gateway selects a pod for the model (least-request,
                                       even without an explicit routing-strategy header)
                                       gateway registers: public job ID → owner, model,
                                       backend job ID, owning pod (namespace/name/UID)
                                       response "id" is rewritten to the public job ID

    GET  /v1/videos/{id}         ──►  public ID resolved, path rewritten to the backend ID,
                                       pinned back to the owning pod; the response "id" is
                                       rewritten back to the public ID
    GET  /v1/videos/{id}/content ──►  same resolve + pin; the body streams through untouched
    DELETE /v1/videos/{id}       ──►  same resolve + pin; the record is deleted once the
                                       backend answers 2xx or 404

    GET  /v1/videos              ──►  answered by the gateway from its own registry: the
                                       caller's job catalog. No backend call, no ``model``
                                       query parameter, no backend IDs.

Records are read from and written to Redis on every operation — there is no per-replica job cache and so no reconciliation window: any replica can serve a follow-up for a job another replica created. Without Redis (local development, a single replica) the registry falls back to process-local storage, and jobs are lost when that replica restarts.

Each record is owner-scoped. The owner is derived from the request's ``user`` header (``scope:user:<name>``); requests without one share ``scope:shared``. A bearer token is not an identity: it authenticates the request but never names a principal. ``GET``/``DELETE`` and the catalog require an exact scope match, and ``scope:shared`` is not a wildcard — so a missing job and someone else's job are both an indistinguishable ``404``.

Resolution also verifies the recorded pod's UID against the informer cache. If the pod is gone, terminating, or has been recreated under a new UID, the job it held is unrecoverable: the record is dropped and the call returns ``404``. A pod that is merely NotReady, or has no routable address yet, keeps its record and returns a retryable ``503``.

Records expire with the backend's own ``expires_at`` when the create response provides one, and after 7 days otherwise — a backend expiry is honoured as given, never stretched to the default, so a create whose ``expires_at`` has already passed is refused with ``503`` rather than registered for a week. Expired records are absent from both lookups and the catalog.

``POST /v1/videos/sync`` and image/audio endpoints don't need any of this: they're a single request/response, so ordinary model-based routing is enough.

----

Prerequisites
-------------

Before you begin, make sure you have:

- A running Kubernetes cluster with `AIBrix installed <https://github.com/vllm-project/aibrix>`_ (includes Envoy Gateway).
- ``kubectl`` configured to talk to your cluster.
- A GPU node (one GPU is enough for the 1.3B-parameter Wan model used below).
- ``jq`` and ``curl`` for the examples below.

----

Step 1 — Deploy the Video Generation Model
-------------------------------------------

.. literalinclude:: ../../../samples/vllm_omni/wan2.1-vace-1.3b.yaml
   :language: yaml

Apply it and wait for the pod to become ready. The model is loaded from the host path ``/data00/models/Wan2.1-VACE-1.3B-diffusers`` (mounted at ``/models``), so the weights must already be present on the node:

.. code-block:: bash

    kubectl apply -f samples/vllm_omni/wan2.1-vace-1.3b.yaml
    kubectl rollout status deployment/wan21-vace-13b

.. tip::
    To serve a different vLLM-Omni model (a different video model, or an image/audio one like ``qwen2-5-omni-7b.yaml``), swap the container's ``vllm serve <model>`` argument, ``--served-model-name``, and the ``model.aibrix.ai/name``/Service name — the request shape stays the same as long as the model belongs to the same task family. See vLLM-Omni's `supported models list <https://docs.vllm.ai/projects/vllm-omni/en/stable/>`_.

----

Step 2 — Access the Gateway
-----------------------------

Port-forward the Envoy service to test locally:

.. code-block:: bash

    export ENVOY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
      --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
      -o jsonpath='{.items[0].metadata.name}')

    kubectl port-forward -n envoy-gateway-system "svc/${ENVOY_SERVICE}" 8080:80

    BASE="http://localhost:8080"

In production, use the LoadBalancer external IP instead:

.. code-block:: bash

    LB_IP=$(kubectl get svc -n envoy-gateway-system \
      -l "gateway.envoyproxy.io/owning-gateway-name=aibrix-eg" \
      -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}')

    BASE="http://${LB_IP}"

----

Step 3 — Generate a Video Synchronously
------------------------------------------

``POST /v1/videos/sync`` blocks until generation completes and returns the raw MP4 bytes directly. Both video endpoints take ``multipart/form-data`` — do **not** set ``-H "Content-Type: application/json"``.

.. code-block:: bash

    curl -X POST "$BASE/v1/videos/sync" \
      --max-time 600 \
      -H "routing-strategy: random" \
      -F "model=wan21-vace-13b" \
      -F "prompt=A drone shot flying over a misty mountain range at sunrise" \
      -F "negative_prompt=blurry, low quality, distorted, static, flickering, artifacts" \
      -F "width=832" \
      -F "height=480" \
      -F "num_frames=81" \
      -F "fps=16" \
      -F "num_inference_steps=30" \
      -F "guidance_scale=5.0" \
      -F "flow_shift=5.0" \
      -F "seed=42" \
      -o output.mp4

**Image-to-video**: VACE also supports using a reference image as the starting frame — pass it via ``input_reference``:

.. code-block:: bash

    curl -X POST "$BASE/v1/videos/sync" \
      --max-time 600 \
      -H "routing-strategy: random" \
      -F "model=wan21-vace-13b" \
      -F "prompt=The mountain landscape in this photo comes alive with drifting clouds and swaying trees" \
      -F "input_reference=@landscape.png" \
      -F "num_frames=81" \
      -F "fps=16" \
      -o output.mp4

----

Step 4 — Generate a Video Asynchronously
--------------------------------------------

For longer generations, ``POST /v1/videos`` returns immediately with a job ID while generation continues in the background — poll for status and fetch the result once it's done. The ID you get back is the gateway's own opaque public job ID (``aibrixjob-...``), not the engine's; use it everywhere below.

**Submit the job:**

.. code-block:: bash

    job_id=$(curl -s "$BASE/v1/videos" \
      -H "user: alice" \
      -F "model=wan21-vace-13b" \
      -F "prompt=A drone shot flying over a misty mountain range at sunrise" \
      -F "negative_prompt=blurry, low quality, distorted, static, flickering, artifacts" \
      -F "width=832" \
      -F "height=480" \
      -F "num_frames=81" \
      -F "fps=16" \
      -F "num_inference_steps=30" \
      -F "guidance_scale=5.0" \
      -F "flow_shift=5.0" \
      -F "seed=42" \
      | jq -r '.id')

    echo "job_id=$job_id"

The ``user`` header is what scopes the job: every follow-up call below must send the same value, or the gateway will answer ``404`` — a job belonging to another scope is indistinguishable from one that does not exist. Omit it consistently and the job lands in the shared scope instead.

The raw response (before extracting ``.id``) looks like this — note that ``id`` is the gateway's public job ID, substituted for the engine's. The same shape is returned by the poll step below, with ``status``, ``progress``, and ``completed_at`` updated as generation proceeds:

.. code-block:: json

    {
      "id": "aibrixjob-9f2c1d7ab34e5f6081924c3d5e7f8a0b",
      "object": "video",
      "model": "wan21-vace-13b",
      "prompt": "A drone shot flying over a misty mountain range at sunrise",
      "status": "queued",
      "size": "832x480",
      "progress": 0,
      "seconds": "5",
      "quality": "default",
      "completed_at": null,
      "created_at": 1788892979,
      "remixed_from_video_id": null,
      "error": null,
      "media_type": "video/mp4",
      "expires_at": null,
      "file_name": null,
      "inference_time_s": null,
      "stage_durations": {},
      "peak_memory_mb": 0.0,
      "action": null
    }

**Poll status** until it reports ``completed`` or ``failed``. The gateway resolves the public ID and transparently pins the request to the pod that created the job, regardless of which pod would normally be picked by its routing policy:

.. code-block:: bash

    watch -n 15 "curl -s -H 'user: alice' $BASE/v1/videos/$job_id | jq ."

**Download the result** once ``completed``:

.. code-block:: bash

    curl -L -H "user: alice" "$BASE/v1/videos/${job_id}/content" -o output.mp4

**List your jobs** instead of polling one by ID. This is the gateway's own catalog for your scope, so no ``model`` parameter is needed. It follows the OpenAI Videos cursor shape: ``limit`` is 1--100 (default 20), ``order`` is ``asc`` or ``desc`` (default ``desc``), and ``after`` is the ``last_id`` from the previous page:

The ``after`` value is a live catalog cursor: the referenced job must still
exist in the same owner scope. If that job is deleted or expires before the next
page is requested, the gateway returns ``400 invalid video list cursor or
parameters``. Restart the listing without ``after``; a public job ID intentionally
contains no creation timestamp from which the deleted position could be recovered.

.. code-block:: bash

    curl -s -H "user: alice" "$BASE/v1/videos?limit=20&order=desc" | jq .

.. code-block:: json

    {
      "object": "list",
      "first_id": "aibrixjob-9f2c1d7ab34e5f6081924c3d5e7f8a0b",
      "has_more": false,
      "last_id": "aibrixjob-9f2c1d7ab34e5f6081924c3d5e7f8a0b",
      "data": [
        {
          "id": "aibrixjob-9f2c1d7ab34e5f6081924c3d5e7f8a0b",
          "object": "video",
          "model": "wan21-vace-13b",
          "created_at": 1788892979,
          "expires_at": 1789497779
        }
      ]
    }

The catalog reports what the gateway knows: the job's identity, model, and retention window. It does not call the backend, so it carries no live ``status`` or ``progress`` — poll the job by ID for those. Video endpoint paths are exact; ``/v1/videos/`` and other trailing-slash variants return ``404``.

**Delete a job** once you no longer need it. The record is removed once the backend confirms the delete (``2xx``) or reports it already gone (``404``); a backend ``5xx`` leaves the record in place so you can retry:

.. code-block:: bash

    curl -X DELETE -H "user: alice" "$BASE/v1/videos/${job_id}"

----

Endpoint Reference
--------------------

.. list-table::
   :header-rows: 1
   :widths: 30 15 55

   * - Endpoint
     - Method
     - Behavior
   * - ``/v1/videos``
     - POST
     - Create an async video generation job. Returns a public job ID immediately, registered against the pod that created it. If the job cannot be registered, the call fails with ``503`` and no ID is handed out.
   * - ``/v1/videos``
     - GET
     - List your own jobs from the gateway's registry. Supports OpenAI-style ``after``, ``limit`` (1--100), and ``order`` (``asc``/``desc``); never calls a backend. A deleted or expired ``after`` cursor returns 400, and the client must restart from the first page.
   * - ``/v1/videos/sync``
     - POST
     - Create a job and block until it completes; returns the video bytes directly. Not registered — there is no follow-up call to route.
   * - ``/v1/videos/{id}``
     - GET
     - Poll a job's status. Pinned to the pod that created it; the response ``id`` is the public one.
   * - ``/v1/videos/{id}/content``
     - GET
     - Download the completed video. Pinned to the pod that created it; the body streams through unbuffered.
   * - ``/v1/videos/{id}``
     - DELETE
     - Delete a job and its stored video. Pinned to the pod that created it; the record is removed after a 2xx or 404 response so a failed delete can still be retried.

----

Troubleshooting
----------------

**``GET``/``DELETE`` on a job ID returns 404**

The ID is unknown to the gateway, has passed its retention window, belongs to a different ``user`` scope than the one on this request, or its owning pod is gone (deleted, terminating, or recreated under a new UID). Once a pod is gone, the video it generated is gone with it — resubmit the job. Note that "not yours" and "does not exist" are deliberately the same answer, so check the ``user`` header before assuming the job expired.

**``GET``/``DELETE`` on a job ID returns 503**

Either the owning pod is temporarily unroutable (NotReady, or no address yet) or the registry's store is unreachable. Both are retryable and the record is kept: retry the call. A ``503`` from ``POST /v1/videos`` means the job could not be registered — the backend may have started work that is now unreachable, so resubmit.

**``POST /v1/videos/sync`` returns 504 / ext_proc timeout**

Synchronous generation can take several minutes. Current AIBrix defaults are 600s for every reserved router's ext_proc ``messageTimeout`` (including the two Videos routes), the ORIGINAL_DST route, and model HTTPRoutes (``AIBRIX_GATEWAY_TIMEOUT_SECONDS``). Older installs used 60s/120s and will abort first; raise those three timeouts to at least 600s.

**``GET /v1/videos`` returns fewer jobs than expected**

The catalog is scoped to the request's ``user`` header, so jobs submitted under a different value (or with no header at all) are not listed. It also lists nothing the gateway did not register: jobs created directly against a pod, and jobs whose retention window has passed, are absent.

**Job created on one gateway replica can't be found via another**

Cross-replica lookup relies on AIBrix's gateway being configured with Redis. Without it the registry is process-local, so the job only exists in the replica that handled the ``POST /v1/videos`` call — and is lost if that replica restarts.

**Video job responses arrive truncated or the client hangs**

The gateway rewrites the job ID inside the create and status responses, so those two bodies leave ext_proc at a different length than they arrived. It waits for the whole body, rewrites it, and drops the upstream ``content-length`` so Envoy re-derives it. That depends on the Videos JSON endpoints riding their own route, ``aibrix-reserved-router-videos``, whose ``EnvoyExtensionPolicy`` sets ``spec.extProc[].processingMode.response.body: Buffered``. An install that removed response-body processing, or that restores ``content-length`` in a later filter, will truncate the rewritten body.

Buffered is what makes the create guarantee real, not just the rewrite: ext_proc holds the response at its headers until the buffered body has been processed, so a backend ``201`` cannot reach the client before the job is durably registered — which is what lets a failed registration still answer ``503``.

The mode has to come from that route's configuration. The pinned Envoy Gateway (v1.2.8) has no ``allowModeOverride`` field on ``EnvoyExtensionPolicy`` and never sets ext_proc's ``allow_mode_override``, so a per-request ``ModeOverride`` from the gateway plugin would be silently ignored; do not add such a field on that version.

Route selection happens twice per request, and both passes matter. The gateway pins a request by answering with ``routing-strategy``/``target-pod`` headers and clearing Envoy's route cache, so the request re-matches the ``EnvoyPatchPolicy`` pinning routes in ``config/gateway/gateway.yaml`` before it leaves for the pod — and the route picked on that second pass is the one Envoy uses for the response. Those pinning routes therefore come in three flavours, one per Videos route family, each enabling the same ext_proc filter its path route did. A single catch-all pinning route would hand every pinned Videos response to the shared, streamed filter, which both loses the buffering and delivers the response to the plugin as a second stream with no request context.

The two endpoints whose bodies must never be buffered — ``/v1/videos/sync`` and ``/v1/videos/{id}/content`` — are matched separately by ``aibrix-reserved-router-videos-streaming`` (an ``Exact`` and a ``RegularExpression`` match, both of which outrank the ``PathPrefix`` ``/v1/videos`` route in Envoy Gateway's match ordering) and keep ``response.body: Streamed``. Neither carries a job ID to rewrite. The shared ``aibrix-reserved-router`` also stays ``Streamed``, because SSE completions have to be forwarded chunk by chunk.

**Pod takes a long time to become ready**

Confirm the weights exist on the node at ``/data00/models/Wan2.1-VACE-1.3B-diffusers``. Loading a video diffusion model into GPU memory can still take a few minutes. Watch progress with:

.. code-block:: bash

    kubectl logs -f deployment/wan21-vace-13b

----

Sample Files
-------------

- `samples/vllm_omni/wan2.1-vace-1.3b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/wan2.1-vace-1.3b.yaml>`_ — Deployment and Service for the video generation example used in this guide
- `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_ — text+audio vLLM-Omni example
