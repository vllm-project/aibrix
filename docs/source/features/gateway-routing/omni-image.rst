.. _omni-image:

=====
Image
=====

This guide covers image generation and editing through AIBrix's gateway. ``POST /v1/images/generations`` and ``POST /v1/images/edits`` are the endpoints AIBrix supports. vLLM-Omni supports both, while SGLang-Omni does not support the image endpoints; see the :ref:`supported endpoints <multimodal-supported-endpoints>`. Before you start, finish the :ref:`prerequisites <omni-prerequisites>` and :ref:`gateway access <omni-access-gateway>` steps on the Multi-Modal Serving page, and deploy your model the way the :ref:`video guide <omni-video-deploy>` describes, swapping in an image model.

----

Generate an Image
-----------------

``POST /v1/images/generations`` is a JSON request. Set ``model`` to the name of your deployment, the value of its ``model.aibrix.ai/name`` label:

.. code-block:: bash

    curl -X POST "$BASE/v1/images/generations" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "my-image-model",
        "prompt": "A watercolor painting of a lighthouse at dawn",
        "n": 1,
        "size": "1024x1024"
      }' \
      -o image.json

What the gateway does with it:

- It reads only ``model``, to select a pod, and forwards the body as sent. ``prompt``, ``size``, ``n`` and every other field are validated by the engine.
- The response is forwarded without being parsed as a language response, so no token usage is reported.
- If the model's pods carry the ``model.aibrix.ai/engine: xdit`` label or annotation, the gateway rewrites the path to xDiT's native ``/generate`` endpoint.

----

Edit an Image
-------------

``POST /v1/images/edits`` is the OpenAI-style image edit endpoint. Unlike generation it takes ``multipart/form-data``, with the source image in an ``image`` part, so — like the ``/v1/videos`` and audio transcription endpoints — do **not** set ``-H "Content-Type: application/json"``:

.. code-block:: bash

    curl -X POST "$BASE/v1/images/edits" \
      -H "Authorization: Bearer ${API_KEY}" \
      -F "model=my-image-edit-model" \
      -F "prompt=Add a red scarf to the cat" \
      -F "image=@cat.png" \
      -o edited.json

What the gateway does with it:

- ``model`` is required as a form field. The gateway needs the name to select a pod, so a request without one is rejected with ``400``.
- A JSON body is rejected with ``400`` and the message ``image edit requests must use multipart/form-data content-type``.
- Every other field, including the ``image`` part, ``prompt``, ``mask_image``, ``n`` and ``size``, is forwarded as sent and validated by the engine.
- A ``stream`` form field set to ``true`` or ``1`` makes the gateway handle the response as an SSE stream. vLLM-Omni accepts it only for multi-stage image editing pipelines.
- Otherwise the response is forwarded without being parsed as a language response, so no token usage is reported.
- The whole request, source images included, counts against the gateway's request size limit, 4 MiB by default; see Message Size Configuration in :doc:`gateway-plugins`. Raise it if your images are larger.
- The path is never rewritten, so the model's engine must serve ``/v1/images/edits`` itself. The ``xdit`` rewrite above applies to generation only.
