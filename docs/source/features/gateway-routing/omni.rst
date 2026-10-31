.. _omni-serving:

===================
Multi-Modal Serving
===================

AIBrix serves multi-modal models through omni and diffusion inference engines, and supports three of them: `vLLM-Omni <https://docs.vllm.ai/projects/vllm-omni/en/stable/>`_, `SGLang-Omni <https://github.com/sgl-project/sglang-omni>`_ and `SGLang Diffusion <https://docs.sglang.io/diffusion/index.html>`_. vLLM-Omni and SGLang-Omni both expose OpenAI-compatible endpoints beyond text chat — speech and general audio and, for vLLM-Omni, image and video generation as well. SGLang Diffusion serves diffusion models and offers image and video generation, but no chat or audio endpoints. AIBrix's gateway understands these endpoints natively. It routes each request to the right backend pods by request path and ``model`` name, whichever engine serves the model, and for video generation it also keeps track of which pod owns each in-progress job so follow-up calls land back on it.

The :ref:`supported endpoints <multimodal-supported-endpoints>` below list what each engine offers and which of those endpoints the AIBrix gateway supports. Setup that every modality shares — deploying a model and reaching the gateway — is covered on this page. The guides then go modality by modality: :doc:`Chat Completions <omni-chat>`, :doc:`Audio <omni-audio>`, :doc:`Image <omni-image>` and :doc:`Video <omni-video>`.

----

.. _multimodal-supported-endpoints:

Supported Endpoints
-------------------

The vLLM-Omni and SGLang columns show what each engine documents upstream. The SGLang column covers two engines, SGLang-Omni and SGLang Diffusion: a plain ✅ means both serve the endpoint, and a ✅ followed by an engine name means only that engine does. The AIBrix column shows whether the AIBrix gateway supports the endpoint, meaning it routes the request to the pods of the requested model.

.. list-table::
   :header-rows: 1
   :widths: 32 16 26 26

   * - API endpoint
     - vLLM-Omni
     - SGLang
     - AIBrix
   * - **Chat & Models**
     -
     -
     -
   * - ``POST /v1/chat/completions``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``POST /v1/chat/completions/batch``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``GET /v1/models``
     - ✅
     - ✅
     - ✅ Supported
   * - **Audio**
     -
     -
     -
   * - ``POST /v1/audio/speech``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``POST /v1/audio/speech/batch``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``GET /v1/audio/speech/stream``
     - ✅
     - ✅ SGLang-Omni
     - ❌ Not supported
   * - ``POST /v1/audio/transcriptions``
     - Model-dependent
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``POST /v1/audio/translations``
     - Model-dependent
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``POST /v1/audio/generate``
     - ✅
     - Not supported
     - ❌ Not supported
   * - ``GET /v1/audio/voices``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``POST /v1/audio/voices``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - ``DELETE /v1/audio/voices/{name}``
     - ✅
     - ✅ SGLang-Omni
     - ✅ Supported
   * - **Image**
     -
     -
     -
   * - ``POST /v1/images/generations``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported
   * - ``POST /v1/images/edits``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported
   * - **Video**
     -
     -
     -
   * - ``POST /v1/videos``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported
   * - ``POST /v1/videos/sync``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``GET /v1/videos/{id}``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported
   * - ``GET /v1/videos/{id}/content``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported
   * - ``DELETE /v1/videos/{id}``
     - ✅
     - ✅ SGLang Diffusion
     - ✅ Supported

In the engine columns, *Model-dependent* means the engine serves the endpoint only for models that support the task, and *Not supported* means the engine, or in the SGLang column both SGLang engines, does not offer the endpoint, going by its upstream documentation. A few AIBrix entries need a word of explanation:

- ``GET /v1/models`` is answered by AIBrix's metadata service from the models it discovers (pod labels and ``ModelAdapter`` resources) instead of being forwarded to an engine, so it lists every model AIBrix knows about, whichever engine serves it.
- ``POST /v1/chat/completions/batch`` is routed like a chat completion, and the engine answers with one JSON document that holds a choice per conversation; see :ref:`omni-chat-batch`.
- ``GET /v1/audio/speech/stream`` is a WebSocket endpoint, and the gateway has no handler for it.
- ``/v1/audio/generate`` does not match any of the gateway's reserved routes.
- The three ``/v1/audio/voices`` calls name no model, so AIBrix reads it from a ``model`` query parameter, and the voices they manage live on the pod that served the upload; see :ref:`omni-audio-voices`.
- SGLang Diffusion's upstream documentation shows ``POST /v1/videos`` taking a JSON body, but the gateway reads the model for that route from a ``multipart/form-data`` body and does not accept JSON there, so send a video create to AIBrix as a form, the way :doc:`omni-video` does.
- AIBrix also serves ``GET /v1/videos`` itself, a catalog of your own video jobs kept in the gateway's registry. It is not an engine endpoint, so it is not listed above; see :ref:`omni-video-endpoint-reference`.

----

How It Works
------------

An omni engine pod is started the same way as any other model in AIBrix — a ``Deployment`` labeled with ``model.aibrix.ai/name`` and a matching ``Service`` — with one engine-specific step. For vLLM-Omni it is passing ``--omni`` to ``vllm serve``: that flag auto-detects the model's task from its architecture and exposes the matching OpenAI-compatible endpoint (``/v1/images/generations``, ``/v1/audio/speech``, ``/v1/videos``, ...). The examples in this guide use vLLM-Omni. For SGLang-Omni and SGLang Diffusion, start the engine as its own documentation describes (``sgl-omni serve`` and ``sglang serve``), set its served model name to the ``model.aibrix.ai/name`` value, and keep the same labels and ``Service``. Sample manifests are at `samples/sglang-omni/qwen3-omni-30b-a3b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/sglang-omni/qwen3-omni-30b-a3b.yaml>`_, `samples/sglang-diffusion/qwen-image.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/sglang-diffusion/qwen-image.yaml>`_ and `samples/sglang-diffusion/wan2.1-t2v-1.3b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/sglang-diffusion/wan2.1-t2v-1.3b.yaml>`_.

Chat, audio and image requests are a single request and response, so ordinary model-based routing is enough for them. Video generation is the exception: a generated video is written to local disk on the pod that created it, so the gateway also tracks each job and routes follow-up calls back to that pod. See :doc:`omni-video` for how that works.

----

.. _omni-prerequisites:

Prerequisites
-------------

Before you begin, make sure you have:

- A running Kubernetes cluster with `AIBrix installed <https://github.com/vllm-project/aibrix>`_ (includes Envoy Gateway).
- ``kubectl`` configured to talk to your cluster.
- A GPU node for the model you deploy.
- ``jq`` and ``curl`` for the examples in the guides.

----

.. _omni-access-gateway:

Access the Gateway
------------------

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

The examples in the guides below use this ``$BASE``.

----

Guides
------

.. toctree::
   :maxdepth: 1

   omni-chat.rst
   omni-audio.rst
   omni-image.rst
   omni-video.rst

.. seealso::

   :doc:`gateway-plugins`
       Gateway routing, the ``user`` header used for job ownership, and message-size limits.
