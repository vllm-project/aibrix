.. _omni-chat:

================
Chat Completions
================

This guide covers multi-modal chat through AIBrix's gateway: ``POST /v1/chat/completions`` with a prompt that mixes text and media, answered by an omni model. The gateway handles it like any other chat completion, including routing, streaming and token usage as described in :doc:`gateway-plugins`, and forwards fields it does not know unchanged. ``GET /v1/models`` lists the models AIBrix serves, and ``POST /v1/chat/completions/batch`` is not supported; see the :ref:`supported endpoints <multimodal-supported-endpoints>`. Before you start, finish the :ref:`prerequisites <omni-prerequisites>` and :ref:`gateway access <omni-access-gateway>` steps on the Multi-Modal Serving page, and deploy your model the way the :ref:`video guide <omni-video-deploy>` describes, swapping in an omni chat model.

.. note::
    A text+audio example (`Qwen/Qwen2.5-Omni-7B <https://huggingface.co/Qwen/Qwen2.5-Omni-7B>`_) lives at `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_ in the AIBrix repository.

----

Ask About an Image
------------------

Send the image as an OpenAI-style content part inside ``messages``. ``modalities`` asks the engine for text-only output:

.. code-block:: bash

    curl -X POST "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "qwen2-5-omni-7b",
        "messages": [{
          "role": "user",
          "content": [
            {"type": "text", "text": "How many cars are there in the picture?"},
            {"type": "image_url", "image_url": {"url": "https://example.com/cars.jpg"}}
          ]
        }],
        "modalities": ["text"],
        "max_tokens": 16
      }'

What the gateway does with it:

- Media travels inside ``messages[].content`` as content parts. The gateway does not decode or validate them; the engine does.
- Fields it does not inspect, such as ``modalities``, are forwarded as sent.
- The gateway writes array-form message content, including inline base64 media, into the text it uses for prefix-cache-aware routing. Prefer a URL over a base64 data URL to keep that text small.
- The request body counts against the gateway's request size limit, 4 MiB by default; see Message Size Configuration in :doc:`gateway-plugins`. A URL also keeps large images under it.
