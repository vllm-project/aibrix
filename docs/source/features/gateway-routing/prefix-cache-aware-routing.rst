.. _prefix_cache_aware_routing:

==========================
Prefix-Cache Aware Routing
==========================

Many LLM requests share a long prompt prefix: a system prompt, a shared document, or the earlier turns of a conversation. An engine that has already processed that prefix still holds its KV cache blocks, so it can skip most of the prefill work. Prefix-cache aware routing sends each request to a pod that already holds the longest matching prefix. This lowers time to first token and saves GPU compute.

The ``prefix-cache`` strategy is built into the AIBrix gateway. In its default mode it needs no changes to your engines, but each engine must have its own prefix caching enabled for the routing to pay off.

.. contents:: On this page
   :local:
   :depth: 2

How it works
------------

For each request, the gateway does the following:

1. Builds the prompt text. For chat requests this is the message text, preceded by the request's ``tools`` when present (see ``AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS``).
2. Tokenizes the text and splits the result into fixed-size blocks. Each block's hash includes the hash of the block before it, so a block matches only when everything before it matches too. A trailing partial block is ignored.
3. Looks the blocks up in a per-model prefix index, in order, and stops at the first block that no ready pod holds. A pod's prefix match is the percentage of the request's blocks, counted from the start, that it holds.
4. Chooses a pod from the prefix match and the pods' load, as described in :ref:`Choosing the pod <prefix_cache_choosing_pod>`.
5. Records the chosen pod as holding the request's blocks, so later requests with the same prefix follow it.

Routing modes
-------------

.. list-table::
   :header-rows: 1
   :widths: 18 40 42

   * - Mode
     - Prefix index
     - Notes
   * - Standard (default)
     - A hash table in each gateway replica. A pod is added to it each time the gateway routes a prefix to that pod.
     - Works with any engine and needs no extra setup. The index reflects where the gateway sent requests, not what the engine still has cached, so it can lag behind cache evictions in the engine.
   * - KV event sync
     - An index fed by the engines' own block-stored and block-removed events. Enabled with ``AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true``.
     - Follows the real cache state of each engine. It requires engines that publish KV cache events and turns on the remote tokenizer automatically. See :doc:`../scaling-performance/kv-event-sync`.

Both modes use the same pod selection.

Enabling the strategy
---------------------

Select it for a single request with the ``routing-strategy`` header:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: prefix-cache" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant. Answer using the attached policy document."},
            {"role": "user", "content": "Summarize section 3."}
        ]
    }'

To make it the default for a model, set ``routingStrategy`` in the model's config profile (see :ref:`config_profiles`), or set the gateway's ``ROUTING_ALGORITHM`` environment variable to ``prefix-cache``. See :doc:`gateway-plugins` for the order in which these apply.

.. _prefix_cache_choosing_pod:

Choosing the pod
----------------

By default the gateway does not route on prefix match alone. It scores every ready pod on two signals, normalizes each score to the range 0 to 1, and sends the request to the pod with the highest weighted total:

* the pod's prefix match, with weight 5, and
* the pod's capacity-aware ``load-balance`` score, with weight 4.

The weights lean toward cache affinity, but load still counts, so the pod with the best prefix match does not always win. Before any strategy runs, the gateway's load-imbalance gate can also narrow the candidates when load across the pods is severely skewed.

If you disable the blend by setting ``AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT=0``, ``prefix-cache`` routes by itself:

1. It sorts the pods that hold a match by prefix match, highest first, and then by running requests, fewest first.
2. It picks the first pod whose running requests are no higher than ``mean + factor × σ``, where the mean and the standard deviation σ are taken over the candidate pods and ``factor`` is ``AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR``. It also skips a pod with more than ``c × (mean + 1)`` running requests, where ``c`` is ``AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR``. At the shipped factor of ``2`` the standard-deviation check cannot skip any pod in a pool of five or fewer; the cap applies at any pool size.
3. If no pod holds a match, or none passes the threshold, it falls back to the pod with the fewest running requests.

``prefix-cache-preble`` is a separate strategy that weighs prefix hits against load using the Preble cost model. See :doc:`gateway-plugins` for all strategies.

Configuration
-------------

Set these variables on the gateway plugin deployment. The defaults are the built-in ones.

.. list-table::
   :header-rows: 1
   :widths: 40 12 48

   * - Variable
     - Default
     - Description
   * - ``AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE``
     - ``character``
     - Local tokenizer: ``character`` (the UTF-8 bytes of the text) or ``tiktoken``.
   * - ``AIBRIX_PREFIX_CACHE_BLOCK_SIZE``
     - ``4``
     - Size of a prefix block, in bytes of the tokenized prompt. With ``tiktoken`` each token counts as 4 bytes. Smaller blocks match finer prefixes but create more blocks to hash and store.
   * - ``AIBRIX_PREFIX_CACHE_BLOCK_NUMBER``
     - ``200000``
     - Maximum number of blocks in the index. The least recently updated blocks are evicted first.
   * - ``AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS``
     - ``20``
     - A block that has not been refreshed for this long expires. A block is refreshed each time the gateway routes a request that contains it. The same duration applies to each pod within a block: a pod that has not been routed the block for this long no longer counts as a match, even while the block stays cached for other pods.
   * - ``AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR``
     - ``1``
     - The ``factor`` in the standalone selection above. A model config profile can override it per request with ``routingConfig.prefixCache.standardDeviationFactor``.
   * - ``AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR``
     - ``1.5``
     - Matched pods with more than ``max(c, 1) × (mean + 1)`` running requests are skipped, where ``c`` is this factor. Applies only when ``prefix-cache`` routes on its own, with the auto-blend off (``AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT=0``). Unlike the standard-deviation check, it applies to pools of any size. A factor between ``0`` and ``1`` is treated as ``1``; ``0``, negative or unparsable values use the default ``1.5``. To effectively disable the cap, set a large value such as ``100``. Lower values give up more prefix-cache affinity for a more even load. Set by environment only.
   * - ``AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS``
     - ``true``
     - Include the chat request's ``tools`` in the hashed text, so requests with the same messages but different tools do not look like a full match.
   * - ``AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER``
     - ``false``
     - Tokenize with the engines' own tokenizer service instead of a local tokenizer.
   * - ``AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED``
     - ``false``
     - Use KV event sync mode.
   * - ``AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_WEIGHT`` and ``AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_LOAD_BALANCE_WEIGHT``
     - ``5`` and ``4``
     - The weights of the prefix match and of ``load-balance`` in the default blend.
   * - ``AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT``
     - ``1``
     - Set to ``0`` to turn the auto-blend off.

The gateway manifest shipped in ``config/gateway/gateway-plugin`` sets ``AIBRIX_PREFIX_CACHE_BLOCK_SIZE`` to ``128`` and ``AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR`` to ``2``, so a default installation runs with those values.

Things to know
--------------

* In standard mode the index is an estimate built from the gateway's own routing decisions. Use KV event sync if you need it to follow the engines' real cache state.
* Each gateway replica keeps its own index. With ``AIBRIX_STATESYNC_ENABLED=true`` the replicas share it through Redis, which they sync periodically, so they converge over time. The replicas then also use the same hash seed (``0`` unless you set ``AIBRIX_PREFIX_CACHE_HASH_SEED``).
* The index is kept per model name. In standard mode it is bounded by the block limit and the expiry time above.
* A prompt prefix shorter than one block cannot match.

Observability
-------------

The strategy exports these Prometheus metrics:

* ``aibrix_prefix_cache_routing_decisions_total``, by prefix-match bucket
* ``aibrix_prefix_cache_routing_selection_total``, by outcome: ``prefix_match``, ``prefix_match_skipped`` or ``least_request_fallback``
* ``aibrix_prefix_cache_routing_latency_seconds``
* ``aibrix_prefix_cache_routing_errors_total``
* ``aibrix_prefix_cache_indexer_status``

The decision, selection, latency and error series are recorded by the standalone selection described above. While the default auto-blend is active, only ``aibrix_prefix_cache_indexer_status`` is updated.

Related pages
-------------

- :doc:`gateway-plugins`: all routing strategies and how to select them.
- :doc:`pd-disaggregation`: PD prefill scoring that reuses this prefix index.
- :doc:`agentic-routing`: keeping the requests of one agent run on the same pod.
- :doc:`../scaling-performance/kv-event-sync`: feeding the prefix index from engine cache events.
- :doc:`../../production/gateway`: Redis and ``AIBRIX_STATESYNC_ENABLED`` for multi-replica indexes.
- :doc:`../../designs/aibrix-gateway`: how the gateway selects a pod.
