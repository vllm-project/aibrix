.. _omni-audio:

=====
Audio
=====

This guide covers the audio endpoints exposed through AIBrix's gateway: text-to-speech, in single and batch form, the custom voices it can speak with, and speech-to-text through transcription and translation. The :ref:`supported endpoints <multimodal-supported-endpoints>` table shows which audio endpoints each engine offers and which of them AIBrix supports. Before you start, finish the :ref:`prerequisites <omni-prerequisites>` and :ref:`gateway access <omni-access-gateway>` steps on the Multi-Modal Serving page, and deploy your model the way the :ref:`video guide <omni-video-deploy>` describes, swapping in an audio model.

.. note::
    A text+audio example (`Qwen/Qwen2.5-Omni-7B <https://huggingface.co/Qwen/Qwen2.5-Omni-7B>`_) lives at `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_ in the AIBrix repository.

----

Text-to-Speech
--------------

``POST /v1/audio/speech`` synthesizes speech from text. It is a JSON request with a binary audio response, so — like the image and synchronous video endpoints — it needs none of the video job machinery: the gateway routes it to the pods of the requested model and streams the audio back untouched.

.. code-block:: bash

    curl -X POST "$BASE/v1/audio/speech" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "qwen2-5-omni-7b",
        "input": "Hello from AIBrix.",
        "voice": "alloy",
        "response_format": "mp3"
      }' \
      -o speech.mp3

What differs from the other endpoints:

- ``model`` is required. The gateway needs the name to select a pod, so a request without one is rejected with ``400``.
- ``input`` is required and cannot be empty. It is the text to synthesize and is also the routing text for prefix-cache-aware routing.
- Every other field, such as ``voice``, ``response_format``, ``speed``, ``instructions``, ``language``, and ``stream_format``, is forwarded as sent and validated by the engine.
- The response is binary audio, which the engine may send in several chunks. The gateway forwards it byte for byte and never parses it as JSON or as an SSE stream, so a request-level ``stream`` field does not turn on SSE handling. Nothing about a successful body is parsed, so no token usage is reported.
- Unlike the ``/v1/videos`` endpoints, this one takes ``application/json``: do **not** send ``multipart/form-data``.

----

Batch Text-to-Speech
--------------------

``POST /v1/audio/speech/batch`` synthesizes several utterances in one call. The request is shared defaults plus an ``items`` array whose entries override them, and the response is **one JSON document** with a base64-encoded result per item — not binary audio. The gateway routes it to the pods of the requested model like ``/v1/audio/speech``; it needs no new route, because the ``/v1/audio/speech`` path match already covers it.

.. code-block:: bash

    curl -X POST "$BASE/v1/audio/speech/batch" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "fishaudio/s2-pro",
        "voice": "default",
        "response_format": "wav",
        "items": [
          {"input": "First sentence."},
          {"input": "Second sentence.", "speed": 1.1}
        ]
      }' \
      -o batch.json

    # Item 0's audio, decoded:
    jq -r '.results[0].audio_data' batch.json | base64 -d > item0.wav

What the gateway checks, and returns ``400`` for:

- The body must be a JSON object, and ``model`` is required, for the same reason as ``/v1/audio/speech``.
- ``items`` is required and cannot be empty, and every entry must be a JSON object. The error's ``param`` names the entry, such as ``items.2``.

Everything inside an item (``input``, ``voice``, ``speed``, ``ref_audio``, ``ref_text``, ...) is forwarded as sent and validated by the engine, and so is the number of items. The engine reports a bad item as a failed entry in an otherwise successful response, so one bad item does not fail the batch:

.. code-block:: json

    {
      "id": "speech-batch-...",
      "results": [
        {"index": 0, "status": "success", "audio_data": "UklGR...", "format": "wav", "media_type": "audio/wav", "finish_reason": "stop"},
        {"index": 1, "status": "error", "error": {"message": "...", "type": "invalid_request_error", "param": "items.1.input"}}
      ],
      "total": 2,
      "succeeded": 1,
      "failed": 1
    }

Things to know when calling it through the gateway:

- The HTTP status is ``200`` even when items fail. Check ``failed`` or each result's ``status``; the gateway does not parse the body, so its metrics cannot see per-item failures.
- Results follow the order of ``items``, and ``index`` is the entry's position in your request.
- The response is forwarded as it arrives and never parsed as JSON or SSE, so no token usage is reported. A request-level ``stream`` field does not turn on SSE handling, and the engine refuses ``stream`` on an item.
- The request body is buffered by the gateway, and the default connection buffer limit is 4 MiB (``bufferLimit``). Give each reference clip as a URL rather than an inline base64 data URL, and keep batches small enough that the response, which inlines every item's audio as base64, stays within what your clients accept.

----

Transcription and Translation
-----------------------------

``POST /v1/audio/transcriptions`` and ``POST /v1/audio/translations`` are the OpenAI-style speech-to-text endpoints, served only for models that support the task. Unlike text-to-speech they take ``multipart/form-data`` with the audio in a ``file`` part, so — like the ``/v1/videos`` endpoints — do **not** set ``-H "Content-Type: application/json"``.

.. code-block:: bash

    curl -X POST "$BASE/v1/audio/transcriptions" \
      -H "Authorization: Bearer ${API_KEY}" \
      -F "model=my-asr-model" \
      -F "file=@speech.wav"

What the gateway checks:

- ``model`` is required as a form field. The gateway needs the name to select a pod, so a request without one is rejected with ``400``.
- A JSON body is rejected with ``400`` and the message ``audio requests must use multipart/form-data content-type``.
- Every other field, including the ``file`` part, is forwarded as sent and validated by the engine.
- A ``stream`` form field set to ``true`` or ``1`` makes the gateway handle the response as an SSE stream.
- The whole request, audio file included, counts against the gateway's request size limit, 4 MiB by default; see Message Size Configuration in :doc:`gateway-plugins`.

----

.. _omni-audio-voices:

Custom Voices
-------------

A text-to-speech engine that clones voices keeps a catalog of voices you upload once and then name in the ``voice`` field of later speech requests. AIBrix routes the three calls that manage it:

- ``GET /v1/audio/voices`` lists the built-in voices and the voices you uploaded.
- ``POST /v1/audio/voices`` uploads a voice sample as ``multipart/form-data``.
- ``DELETE /v1/audio/voices/{name}`` removes an uploaded voice.

These calls name no model, because an engine serves a single model and needs none, and a ``GET`` or ``DELETE`` has no body to carry one. The gateway chooses a pod by model, so it reads the model from a ``model`` **query parameter**, on all three calls:

.. code-block:: bash

    # List the voices
    curl "$BASE/v1/audio/voices?model=my-tts-model" \
      -H "Authorization: Bearer ${API_KEY}"

    # Upload a voice
    curl -X POST "$BASE/v1/audio/voices?model=my-tts-model" \
      -H "Authorization: Bearer ${API_KEY}" \
      -F "name=narrator" \
      -F "consent=consent-1" \
      -F "audio_sample=@narrator.wav" \
      -F "ref_text=Transcript of the sample."

    # Delete it
    curl -X DELETE "$BASE/v1/audio/voices/narrator?model=my-tts-model" \
      -H "Authorization: Bearer ${API_KEY}"

What the gateway checks:

- ``model`` is required, must be given once, and must name a model AIBrix knows. A request that breaks any of these is rejected with ``400``, and the error's ``param`` is ``model``. A known model with no ready pod is answered with ``503``.
- Everything else is the engine's: the form fields of an upload (``name``, ``consent``, ``audio_sample``, ``ref_text``, ``speaker_description``), the voice names, the status codes, and the responses. The query string reaches the engine as sent, ``model`` included, which the engine ignores.
- The responses are small JSON documents. The gateway forwards them unparsed, so no token usage is reported.
- An upload takes ``multipart/form-data``, so — as with transcription — do **not** set ``-H "Content-Type: application/json"``. The whole request, voice sample included, counts against the gateway's request size limit, 4 MiB by default; see Message Size Configuration in :doc:`gateway-plugins`.

.. warning::
    The engine keeps an uploaded voice on the pod that took the upload (vLLM-Omni writes it under ``SPEAKER_SAMPLES_DIR``, ``~/.cache/vllm-omni/speakers`` by default), and the gateway sends each call to one pod of the model. With more than one replica, a later list, delete or speech request can therefore reach a pod that has never seen the voice. Serve the model from a single replica, or give every replica the same ``SPEAKER_SAMPLES_DIR`` on a volume they share.

----

.. _omni-audio-endpoint-reference:

Endpoint Reference
------------------

.. list-table::
   :header-rows: 1
   :widths: 30 15 55

   * - Endpoint
     - Method
     - Behavior
   * - ``/v1/audio/speech``
     - POST
     - JSON request, binary audio response. Routed by model like a chat completion; the audio streams through unparsed, so no token usage is recorded and no job is registered.
   * - ``/v1/audio/speech/batch``
     - POST
     - JSON request with an ``items`` array, one JSON response holding a base64 result per item. Routed by model; the gateway checks only ``model`` and ``items``, the response streams through unparsed, and ``200`` does not mean every item succeeded.
   * - ``/v1/audio/transcriptions``, ``/v1/audio/translations``
     - POST
     - ``multipart/form-data`` request with the model in a ``model`` form field. Routed by model; a ``stream`` form field set to ``true`` or ``1`` makes the response an SSE stream.
   * - ``/v1/audio/voices``, ``/v1/audio/voices/{name}``
     - GET, POST, DELETE
     - List, upload (``multipart/form-data``) and delete custom voices. Routed by the model in a ``model`` query parameter, which all three calls need; the responses stream through unparsed, and a voice lives only on the pod that took its upload.

----

Sample Files
------------

- `samples/vllm_omni/qwen2-5-omni-7b.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/vllm_omni/qwen2-5-omni-7b.yaml>`_ — text+audio vLLM-Omni example

.. seealso::

   :doc:`gateway-plugins`
       Gateway routing, the ``user`` header used for job ownership, and message-size limits.
