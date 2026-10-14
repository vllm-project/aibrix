.. _omni-chat:

================
Chat Completions
================

This guide covers multi-modal chat through AIBrix's gateway: ``POST /v1/chat/completions`` with a prompt that mixes text and media, answered by an omni model. The gateway handles it like any other chat completion, including routing, streaming and token usage as described in :doc:`gateway-plugins`, and forwards fields it does not know unchanged. ``GET /v1/models`` lists the models AIBrix serves, and ``POST /v1/chat/completions/batch`` answers several conversations in one call, as :ref:`below <omni-chat-batch>`; see the :ref:`supported endpoints <multimodal-supported-endpoints>`. Before you start, finish the :ref:`prerequisites <omni-prerequisites>` and :ref:`gateway access <omni-access-gateway>` steps on the Multi-Modal Serving page, and deploy your model the way the :ref:`video guide <omni-video-deploy>` describes, swapping in an omni chat model.

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

----

.. _omni-chat-batch:

Batch Chat Completions
----------------------

``POST /v1/chat/completions/batch`` answers several conversations in one call. It is the chat completions request with one difference: ``messages`` is a list of conversations, each a list of messages. The response is **one non-streaming JSON document**, a chat completion with one choice per conversation (``choices[i]`` answers ``messages[i]``) and ``usage`` summed over the whole batch. vLLM-Omni serves it; SGLang-Omni does not.

.. code-block:: bash

    curl -X POST "$BASE/v1/chat/completions/batch" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "qwen2-5-omni-7b",
        "messages": [
          [{"role": "user", "content": "What is the capital of France?"}],
          [{"role": "user", "content": "What is the capital of Japan?"}]
        ],
        "max_tokens": 16
      }'

What the gateway does with it:

- It needs no new route: the ``/v1/chat/completions`` path match already covers it, and the gateway routes the whole batch to one pod of the requested model, like a single chat completion.
- It checks only the envelope: a non-empty ``model`` and a non-empty ``messages`` list whose entries are non-empty lists of messages. A flat list of messages, the single-conversation shape, is rejected with ``400`` naming ``messages.0``. Everything else, including message contents and sampling options, is left to the engine.
- The conversations are joined into one text for prefix-cache-aware routing, and the request body is forwarded as sent.
- The gateway never treats a batch as a streaming (SSE) request, whatever ``stream`` says, and handles the response as one JSON document. The engine does not stream a batch either and may reject ``stream: true``, so send ``stream`` as ``false`` or leave it out. The engine also refuses ``n`` greater than 1 and beam search.
- Token usage is read from the summed ``usage``, so a user's TPM limit is charged for the whole batch.
- The ``pd`` routing strategy is not supported for this endpoint and is rejected with ``400``.
