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

The stock gateway already matches ``/v1/videos`` on the reserved HTTPRoute (so requests reach ext_proc before a ``model`` header exists) and uses 600s Envoy / ext_proc / model-HTTPRoute timeouts so synchronous generation can finish. If you installed an older AIBrix release, add a PathPrefix ``/v1/videos`` match to ``aibrix-reserved-router`` and set ``AIBRIX_GATEWAY_TIMEOUT_SECONDS=600`` on the controller-manager.

Video generation needs a bit more from the gateway than a stateless chat completion does. Because a generated video is written to local disk on whichever pod created it, the gateway has to remember that pod-to-job mapping and route follow-up calls back to the exact same pod:

.. code-block:: text

    POST /v1/videos              ──►  routed to any ready pod for the model (normal routing)
                                       gateway records: video_id → owning pod

    GET  /v1/videos/{id}         ──►  pinned back to the owning pod (only it has the job)
    GET  /v1/videos/{id}/content ──►  pinned back to the owning pod
    DELETE /v1/videos/{id}       ──►  pinned back to the owning pod, mapping evicted

    GET  /v1/videos?model=X      ──►  gateway fans out to every ready pod for X and merges
                                       their individual job lists (no single owning pod for "list")

This mapping is kept in-memory per gateway replica and, when the gateway is configured with Redis, written through to Redis too — so a job created via one gateway replica can still be polled or fetched through a different replica. An unknown, expired, or no-longer-live video ID returns ``404`` rather than routing nowhere.

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

For longer generations, ``POST /v1/videos`` returns immediately with a job ID while generation continues in the background — poll for status and fetch the result once it's done.

**Submit the job:**

.. code-block:: bash

    video_id=$(curl -s "$BASE/v1/videos" \
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

    echo "video_id=$video_id"

The raw response (before extracting ``.id``) looks like this. The same shape is returned by the poll step below, with ``status``, ``progress``, and ``completed_at`` updated as generation proceeds:

.. code-block:: json

    {
      "id": "video_gen_cf8bcaf5bccc43f6926a689c9bb7312a",
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

**Poll status** until it reports ``completed`` or ``failed``. This request is transparently pinned to the pod that created the job, regardless of which pod would normally be picked by the gateway's routing policy:

.. code-block:: bash

    watch -n 15 "curl -s $BASE/v1/videos/$video_id | jq ."

**Download the result** once ``completed``:

.. code-block:: bash

    curl -L "$BASE/v1/videos/${video_id}/content" -o output.mp4

**List all jobs for a model** instead of polling one by ID (``model`` is required):

.. code-block:: bash

    curl -s "$BASE/v1/videos?model=wan21-vace-13b" | jq .

**Delete a job** once you no longer need it:

.. code-block:: bash

    curl -X DELETE "$BASE/v1/videos/${video_id}"

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
     - Create an async video generation job. Returns a job ID immediately.
   * - ``/v1/videos``
     - GET
     - List jobs for a model. Requires a ``model`` query parameter; fans out across every ready pod for that model.
   * - ``/v1/videos/sync``
     - POST
     - Create a job and block until it completes; returns the video bytes directly.
   * - ``/v1/videos/{id}``
     - GET
     - Poll a job's status. Pinned to the pod that created it.
   * - ``/v1/videos/{id}/content``
     - GET
     - Download the completed video. Pinned to the pod that created it.
   * - ``/v1/videos/{id}``
     - DELETE
     - Delete a job and its stored video. Pinned to the pod that created it; the mapping is evicted after a 2xx or 404 response so a failed delete can still be retried.

----

Troubleshooting
----------------

**``GET``/``DELETE`` on a video ID returns 404**

The job ID is unknown to the gateway, has passed its retention window, or its owning pod is no longer ready (e.g. it was rescheduled). Once a pod is gone, the video it generated is gone with it — resubmit the job.

**``POST /v1/videos/sync`` returns 504 / ext_proc timeout**

Synchronous generation can take several minutes. Current AIBrix defaults are 600s for the reserved-router ext_proc ``messageTimeout``, the ORIGINAL_DST route, and model HTTPRoutes (``AIBRIX_GATEWAY_TIMEOUT_SECONDS``). Older installs used 60s/120s and will abort first; raise those three timeouts to at least 600s.

**``GET /v1/videos`` (list) returns 400**

The ``model`` query parameter is required — the gateway needs it to know which pods to fan out to: ``curl -s "$BASE/v1/videos?model=wan21-vace-13b"``.

**Job created on one gateway replica can't be found via another**

This cross-replica lookup relies on AIBrix's gateway being configured with Redis. Without Redis, the video-job-to-pod mapping only lives in the replica that handled the ``POST /v1/videos`` call.

**Pod takes a long time to become ready**

Confirm the weights exist on the node at ``/data00/models/Wan2.1-VACE-1.3B-diffusers``. Loading a video diffusion model into GPU memory can still take a few minutes. Watch progress with:

.. code-block:: bash

    kubectl logs -f deployment/wan21-vace-13b

----

Sample Files
-------------

- `samples/vllm_omni/wan2.1-vace-1.3b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/wan2.1-vace-1.3b.yaml>`_ — Deployment and Service for the video generation example used in this guide
- `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_ — text+audio vLLM-Omni example
