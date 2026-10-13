.. _omni-image:

=====
Image
=====

This guide covers image generation through AIBrix's gateway. ``POST /v1/images/generations`` is the endpoint AIBrix supports. vLLM-Omni supports it, while SGLang-Omni does not support the image endpoints; see the :ref:`supported endpoints <multimodal-supported-endpoints>`. ``POST /v1/images/edits`` is not supported by the AIBrix gateway. Before you start, finish the :ref:`prerequisites <omni-prerequisites>` and :ref:`gateway access <omni-access-gateway>` steps on the Multi-Modal Serving page, and deploy your model the way the :ref:`video guide <omni-video-deploy>` describes, swapping in an image model.

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
