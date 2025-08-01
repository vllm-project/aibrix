========================
KV Cache Events Guide
========================

Overview
--------

KV Cache Event Synchronization is a feature that enables multiple vLLM instances to share key-value cache states through ZMQ-based event publishing. This improves prefix cache hit rates and reduces redundant computation by allowing the AIBrix gateway to make intelligent routing decisions based on real-time cache state.

Architecture
------------

The KV event synchronization system consists of:

1. **vLLM Instances**: Publish KV cache events via ZMQ pub/sub pattern
2. **AIBrix Cache**: Manages subscriptions and processes events
3. **Sync Prefix Cache Indexer**: Maintains global prefix cache state
4. **Gateway Router**: Uses cache state for intelligent routing decisions

Event Flow
~~~~~~~~~~

.. code-block:: text

   vLLM Pod 1 ─────┐
                    ├─── ZMQ Events ───► KV Event Manager ───► Sync Indexer ───► Gateway Router
   vLLM Pod N ─────┘                         (in Cache)

The system uses a two-stage initialization:

1. **Cache Initialization**: Using ``InitWithOptions`` pattern with ``EnableKVSync=true``
2. **KV Event Manager**: Automatically created when conditions are met

Requirements
------------

- vLLM version 0.7.0 or later with KV cache events support
- AIBrix gateway-plugins built with ZMQ support (``-tags="zmq"``)
- ZMQ library (libzmq3-dev) installed on gateway nodes
- Remote tokenizer enabled (strict prerequisite)
- Redis client configured (for production deployments)

.. important::
   KV event sync has a strict dependency on remote tokenizer to ensure consistent tokenization between gateway and vLLM instances. The system will not initialize if remote tokenizer is disabled.

Configuration
-------------

Environment Variables
~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 40 20 40

   * - Variable
     - Default
     - Description
   * - ``AIBRIX_KV_EVENT_SYNC_ENABLED``
     - ``false``
     - Enable KV event synchronization
   * - ``AIBRIX_USE_REMOTE_TOKENIZER``
     - ``false``
     - Must be ``true`` for KV sync
   * - ``AIBRIX_REMOTE_TOKENIZER_ENDPOINT``
     - -
     - vLLM service endpoint
   * - ``AIBRIX_PREFIX_CACHE_METRICS_ENABLED``
     - ``false``
     - Enable prefix cache metrics

Pod Labels
~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 40 20 40

   * - Label
     - Value
     - Description
   * - ``model.aibrix.ai/kv-events-enabled``
     - ``true``
     - Enable KV events for this pod
   * - ``model.aibrix.ai/lora-id``
     - string
     - LoRA adapter ID (optional)

vLLM Configuration
~~~~~~~~~~~~~~~~~~

Add these arguments to your vLLM container:

.. code-block:: yaml

   args:
     - --enable-kv-cache-events
     - --kv-events-publisher=zmq
     - --kv-events-endpoint=tcp://*:5557
     - --kv-events-replay-endpoint=tcp://*:5558
     - --kv-events-buffer-steps=10000

Add corresponding ports:

.. code-block:: yaml

   ports:
     - name: kv-events
       containerPort: 5557
       protocol: TCP
     - name: kv-replay
       containerPort: 5558
       protocol: TCP

Deployment
----------

Quick Start
~~~~~~~~~~~

1. **Enable Remote Tokenizer** (mandatory prerequisite)::

      kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
        AIBRIX_USE_REMOTE_TOKENIZER=true \
        AIBRIX_REMOTE_TOKENIZER_ENDPOINT=http://vllm-service:8000

2. **Enable KV Event Sync**::

      kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
        AIBRIX_KV_EVENT_SYNC_ENABLED=true

3. **Enable Prefix Cache Metrics** (optional but recommended)::

      kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
        AIBRIX_PREFIX_CACHE_METRICS_ENABLED=true

3. **Deploy vLLM with KV Events**:

   .. code-block:: yaml

      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: vllm-model
      spec:
        template:
          metadata:
            labels:
              model.aibrix.ai/name: "llama-7b"
              model.aibrix.ai/kv-events-enabled: "true"
          spec:
            containers:
            - name: vllm
              args:
              - --enable-kv-cache-events
              - --kv-events-publisher=zmq
              - --kv-events-endpoint=tcp://*:5557
              - --kv-events-replay-endpoint=tcp://*:5558

Build Considerations
~~~~~~~~~~~~~~~~~~~~

AIBrix uses conditional compilation to manage ZMQ dependencies:

**Components requiring ZMQ support:**

- ``gateway-plugins``: Main component for KV event sync
- ``kvcache-watcher``: Optional component for cache monitoring

**Build commands:**

.. code-block:: bash

   # Build with ZMQ support
   go build -tags="zmq" ./cmd/plugins/main.go
   
   # Docker build with ZMQ
   make docker-build-gateway-plugins  # Automatically includes ZMQ

**Components that do NOT require ZMQ:**

- ``controller-manager``: Uses default build
- ``metadata-service``: Uses default build
- ``runtime``: Python component, no ZMQ needed

Event Types
-----------

BlockStoredEvent
~~~~~~~~~~~~~~~~

Published when new KV cache blocks are stored:

.. code-block:: go

   type BlockStoredEvent struct {
       BlockHashes  []int64    // Hash values of stored blocks
       TokenIDs     [][]int32  // Token IDs for each block
       ModelName    string     // Model identifier
       LoraID       int64      // LoRA adapter ID (-1 if none)
       PodName      string     // Source pod name
   }

BlockRemovedEvent
~~~~~~~~~~~~~~~~~

Published when blocks are removed from cache:

.. code-block:: go

   type BlockRemovedEvent struct {
       BlockHashes  []int64    // Hash values of removed blocks
       ModelName    string     // Model identifier
       LoraID       int64      // LoRA adapter ID
       PodName      string     // Source pod name
   }

Troubleshooting
---------------

Initialization Failures
~~~~~~~~~~~~~~~~~~~~~~~

1. **Check initialization logs**::

      kubectl logs deployment/aibrix-gateway-plugins -n aibrix-system | grep -E "KV event|initialize cache"

2. **Verify remote tokenizer**::

      # Must see both enabled
      kubectl get deployment/aibrix-gateway-plugins -n aibrix-system -o yaml | grep -A2 "REMOTE_TOKENIZER\|KV_EVENT_SYNC"

Events Not Publishing
~~~~~~~~~~~~~~~~~~~~~

1. **Check vLLM logs**::

      kubectl logs deployment/vllm-model | grep "KV cache events"

2. **Verify ZMQ connectivity**::

      kubectl exec -it <gateway-pod> -n aibrix-system -- nc -zv <vllm-pod-ip> 5557

3. **Check ZMQ build support**::

      kubectl exec <gateway-pod> -n aibrix-system -- ldd /app/gateway-plugin | grep zmq

Connection Issues
~~~~~~~~~~~~~~~~~

1. **Verify pod labels**::

      kubectl get pods -l model.aibrix.ai/kv-events-enabled=true

2. **Check network policies**:

   - Ensure ports 5557-5558 are accessible
   - No blocking NetworkPolicies

3. **Validate tokenizer**::

      kubectl exec <gateway-pod> -- curl http://tokenizer:8080/health

Performance Tuning
~~~~~~~~~~~~~~~~~~

- **High Memory Usage**: Reduce buffer steps in vLLM
- **Event Processing Lag**: Adjust batch size and polling timeout
- **Network Overhead**: ~1MB/s per pod at high load

Migration from Existing Deployments
-----------------------------------

Enable on Existing vLLM
~~~~~~~~~~~~~~~~~~~~~~~

1. Add labels::

      kubectl label deployment vllm-model model.aibrix.ai/kv-events-enabled=true

2. Update deployment with KV event args (see Configuration section)

3. Restart pods::

      kubectl rollout restart deployment vllm-model

Rollback
~~~~~~~~

To disable KV event sync::

   # Disable in gateway
   kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
     AIBRIX_KV_EVENT_SYNC_ENABLED=false

   # Remove from vLLM deployments
   kubectl label deployment vllm-model model.aibrix.ai/kv-events-enabled-

Best Practices
--------------

1. **Deployment Order**: 
   
   - Enable remote tokenizer first and verify it's working
   - Deploy vLLM with KV events configuration
   - Enable KV sync in gateway last

2. **Monitoring**:
   
   - Enable prefix cache metrics for visibility
   - Monitor ZMQ connection status in logs
   - Track prefix cache hit rates in Grafana

3. **Resource Planning**:
   
   - ZMQ traffic: ~1MB/s per vLLM pod at high load
   - Memory: Sync indexer uses ~64 bytes per prefix entry
   - CPU: Minimal overhead (<1% per pod)

4. **Production Considerations**:
   
   - Use dedicated network for ZMQ traffic if possible
   - Configure appropriate timeouts based on network latency
   - Plan for graceful degradation if KV sync fails