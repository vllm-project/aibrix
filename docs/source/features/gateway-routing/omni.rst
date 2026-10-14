.. _omni-serving:

===================
Multi-Modal Serving
===================

AIBrix serves multi-modal models through omni inference engines, and supports two of them: `vLLM-Omni <https://docs.vllm.ai/projects/vllm-omni/en/stable/>`_ and `SGLang-Omni <https://github.com/sgl-project/sglang-omni>`_. Both expose OpenAI-compatible endpoints beyond text chat — speech and general audio and, for vLLM-Omni, image and video generation as well. AIBrix's gateway understands these endpoints natively. It routes each request to the right backend pods by request path and ``model`` name, whichever engine serves the model, and for video generation it also keeps track of which pod owns each in-progress job so follow-up calls land back on it.

The :ref:`supported endpoints <multimodal-supported-endpoints>` below list what each engine offers and which of those endpoints the AIBrix gateway supports. Setup that every modality shares — deploying a model and reaching the gateway — is covered on this page. The guides then go modality by modality: :doc:`Chat Completions <omni-chat>`, :doc:`Audio <omni-audio>`, :doc:`Image <omni-image>` and :doc:`Video <omni-video>`.

----

.. _multimodal-supported-endpoints:

Supported Endpoints
-------------------

The vLLM-Omni and SGLang-Omni columns show what each engine documents upstream. The AIBrix column shows whether the AIBrix gateway supports the endpoint, meaning it routes the request to the pods of the requested model.

.. list-table::
   :header-rows: 1
   :widths: 34 20 22 24

   * - API endpoint
     - vLLM-Omni
     - SGLang-Omni
     - AIBrix
   * - **Chat & Models**
     -
     -
     -
   * - ``POST /v1/chat/completions``
     - ✅
     - ✅
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
     - ✅
     - ✅ Supported
   * - ``POST /v1/audio/speech/batch``
     - ✅
     - ✅
     - ✅ Supported
   * - ``GET /v1/audio/speech/stream``
     - ✅
     - ✅
     - ❌ Not supported
   * - ``POST /v1/audio/transcriptions``
     - Model-dependent
     - ✅
     - ✅ Supported
   * - ``POST /v1/audio/translations``
     - Model-dependent
     - ✅
     - ✅ Supported
   * - ``POST /v1/audio/generate``
     - ✅
     - Not supported
     - ❌ Not supported
   * - ``GET /v1/audio/voices``
     - ✅
     - ✅
     - ❌ Not supported
   * - ``POST /v1/audio/voices``
     - ✅
     - ✅
     - ❌ Not supported
   * - ``DELETE /v1/audio/voices/{name}``
     - ✅
     - ✅
     - ❌ Not supported
   * - **Image**
     -
     -
     -
   * - ``POST /v1/images/generations``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``POST /v1/images/edits``
     - ✅
     - Not supported
     - ❌ Not supported
   * - **Video**
     -
     -
     -
   * - ``POST /v1/videos``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``POST /v1/videos/sync``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``GET /v1/videos/{id}``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``GET /v1/videos/{id}/content``
     - ✅
     - Not supported
     - ✅ Supported
   * - ``DELETE /v1/videos/{id}``
     - ✅
     - Not supported
     - ✅ Supported

In the engine columns, *Model-dependent* means the engine serves the endpoint only for models that support the task, and *Not supported* means the engine does not offer the endpoint, going by its upstream documentation. A few AIBrix entries need a word of explanation:

- ``GET /v1/models`` is answered by AIBrix's metadata service from the models it discovers (pod labels and ``ModelAdapter`` resources) instead of being forwarded to an engine, so it lists every model AIBrix knows about, whichever engine serves it.
- ``POST /v1/chat/completions/batch`` is routed like a chat completion, and the engine answers with one JSON document that holds a choice per conversation; see :ref:`omni-chat-batch`.
- ``GET /v1/audio/speech/stream`` is a WebSocket endpoint, and the gateway has no handler for it.
- ``/v1/audio/generate``, ``/v1/audio/voices`` and ``/v1/images/edits`` do not match any of the gateway's reserved routes.
- AIBrix also serves ``GET /v1/videos`` itself, a catalog of your own video jobs kept in the gateway's registry. It is not an engine endpoint, so it is not listed above; see :ref:`omni-video-endpoint-reference`.

----

How It Works
------------

An omni engine pod is started the same way as any other model in AIBrix — a ``Deployment`` labeled with ``model.aibrix.ai/name`` and a matching ``Service`` — with one engine-specific step. For vLLM-Omni it is passing ``--omni`` to ``vllm serve``: that flag auto-detects the model's task from its architecture and exposes the matching OpenAI-compatible endpoint (``/v1/images/generations``, ``/v1/audio/speech``, ``/v1/videos``, ...). The examples in this guide use vLLM-Omni. For SGLang-Omni, start the engine as its own documentation describes and keep the same labels and ``Service``.

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
