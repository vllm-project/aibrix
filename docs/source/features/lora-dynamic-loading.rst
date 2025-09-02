.. _lora:

====================
Lora Dynamic Loading
====================

The LoRA Model Adapter support is crucial for improving model density and reducing inference costs in large language models (LLMs).
By enabling dynamic loading and unloading of LoRA adapters, it allows multiple models to be deployed more efficiently on a shared infrastructure.

This reduces resource consumption by reusing base models across different tasks while swapping in lightweight LoRA adapters for specific model tuning.
The approach optimizes GPU memory usage, decreases cold start times, and enhances scalability, making it ideal for high-density deployment and cost-effective inference in production environments.

High Level Design
-----------------

Model Adapter Controller
^^^^^^^^^^^^^^^^^^^^^^^^

We develop a ModelAdapter `controller <https://kubernetes.io/docs/concepts/architecture/controller/>`_ to manage the lora model adapter lifecycle.
User can submit a lora `Custom Resource <https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/>`_ and controller will register to the matching pods, configure the service discovery and expose the adapter status.

.. figure:: ../assets/images/lora-controller-workflow.png
  :alt: lora-controller-workflow
  :width: 70%
  :align: center

ModelAdapter Lifecycle and Phase Transitions
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The ModelAdapter goes through several distinct phases during its lifecycle. Understanding these phases helps users monitor and troubleshoot their LoRA deployments:

**Phase Transition Flow:**

::

    Pending → Scheduled → Loading → Bound → Running
       ↓         ↓         ↓        ↓        ↓
    Starting  Pod      Adapter   Service   Ready for
    reconcile Selected  Loading   Created   Inference

**Phase Details:**

1. **Pending**: Initial state when the ModelAdapter is first created. The controller starts reconciliation and validates the configuration.

2. **Scheduled**: The controller has successfully identified and selected suitable pods that match the ``podSelector`` criteria. Pods are validated for readiness and stability before selection.

3. **Loading**: The controller is actively loading the LoRA adapter onto the selected pods. This includes:
   
   - Downloading the adapter from the specified ``artifactURL`` 
   - Registering the adapter with the vLLM engine
   - Handling retry mechanisms with exponential backoff if loading fails

4. **Bound**: The LoRA adapter has been successfully loaded onto the pods and the controller is creating the associated Kubernetes Service and EndpointSlice resources for service discovery.

5. **Running**: The ModelAdapter is fully operational and ready to serve inference requests. The LoRA model is accessible through the gateway using the adapter name.

**Error Handling and Reliability Features:**

- **Retry Mechanism**: Up to 5 retry attempts per pod with exponential backoff (starting at 5 seconds)
- **Pod Switching**: Automatic selection of alternative pods if loading fails on initial pods  
- **Pod Health Validation**: Ensures pods are ready and stable before scheduling adapters
- **Connection Error Handling**: Graceful handling of common startup errors like "connection refused"

**Monitoring Phase Transitions:**

You can monitor the current phase and detailed status using:

.. code-block:: bash

    kubectl describe modeladapter <adapter-name>

The status section will show the current phase and transition history with timestamps and reasons for each state change.

Model Adapter Service Discovery
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

We aim to reuse the `Kubernetes Service <https://kubernetes.io/docs/concepts/services-networking/service/>`_ as the abstraction layer for each lora model.
Traditionally, a single pod belongs to one service. However, for LoRA scenarios, we have multiple lora adapters in one pod which breaks kubernete native design.
To support lora cases in kubernetes native way, we customize the lora endpoints and allow a single pod with different LoRAs belong to multiple services.

vLLM Engine Changes
^^^^^^^^^^^^^^^^^^^

High density Lora support can not be done solely in the control plane side, we also write an RFC about improving the
**Visibility of LoRA metadata**, **Dynamic Loading and Unloading**, **Remote Registry Support**, **Observability**
to enhance LoRA management for production grade serving. Please check `[RFC]: Enhancing LoRA Management for Production Environments in vLLM <https://github.com/vllm-project/vllm/issues/6275>`_ for more details.
Majority of the changes are done in vLLM, we just need to use the latest vLLM image and that would be ready for production.


Examples
--------

Here's one example of how to create a lora adapter.

Prerequisites
^^^^^^^^^^^^^

1. You have a base model deployed in the same namespace. 
2. vLLM engine needs to enable `VLLM_ALLOW_RUNTIME_LORA_UPDATING <https://docs.vllm.ai/en/stable/features/lora.html#dynamically-serving-lora-adapters>`_ feature flag.
3. You have a lora model hosted on Huggingface or S3 compatible storage.


Create base model
^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../samples/adapter/base.yaml
   :language: yaml

.. code-block:: bash

    # Expose endpoint
    LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
    ENDPOINT="${LB_IP}:80"

    # send request to base model
    curl -v http://${ENDPOINT}/v1/completions \
        -H "Content-Type: application/json" \
        -d '{
            "model": "qwen-coder-1-5b-instruct",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'

Create lora model adapter
^^^^^^^^^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../samples/adapter/adapter.yaml
   :language: yaml

If you run ```kubectl describe modeladapter qwen-code-lora``, you will see the status of the lora adapter progressing through different phases:

**Phase 1: Pending**

.. code-block:: bash

    $ kubectl describe modeladapter qwen-code-lora
    Status:
      Conditions:
        Last Transition Time:  2025-02-16T19:14:50Z
        Message:               Starting reconciliation
        Reason:                ModelAdapterPending
        Status:                Unknown
        Type:                  Initialized
      Phase:  Pending

**Phase 2: Scheduled**

.. code-block:: bash

    $ kubectl describe modeladapter qwen-code-lora  
    Status:
      Conditions:
        Last Transition Time:  2025-02-16T19:14:50Z
        Message:               Starting reconciliation
        Reason:                ModelAdapterPending
        Status:                Unknown
        Type:                  Initialized
        Last Transition Time:  2025-02-16T19:14:52Z
        Message:               ModelAdapter default/qwen-code-lora has selected 1 pods for scheduling: [qwen-coder-1-5b-instruct-5587f4c57d-kml6s]
        Reason:                Scheduled
        Status:                True
        Type:                  Scheduled
      Phase:  Scheduled

**Phase 3-5: Loading → Bound → Running**

.. code-block:: bash

    $ kubectl describe modeladapter qwen-code-lora
    Status:
      Conditions:
        Last Transition Time:  2025-02-16T19:14:50Z
        Message:               Starting reconciliation
        Reason:                ModelAdapterPending
        Status:                Unknown
        Type:                  Initialized
        Last Transition Time:  2025-02-16T19:14:52Z
        Message:               ModelAdapter default/qwen-code-lora has selected 1 pods for scheduling: [qwen-coder-1-5b-instruct-5587f4c57d-kml6s]
        Reason:                Scheduled
        Status:                True
        Type:                  Scheduled
        Last Transition Time:  2025-02-16T19:14:55Z
        Message:               ModelAdapter default/qwen-code-lora is ready
        Reason:                ModelAdapterAvailable
        Status:                True
        Type:                  Ready
      Instances:
        qwen-coder-1-5b-instruct-5587f4c57d-kml6s
      Phase:  Running

Send request using lora model name to the gateway.

.. code-block:: bash

    # send request to base model
    curl -v http://${ENDPOINT}/v1/completions \
        -H "Content-Type: application/json" \
        -d '{
            "model": "qwen-code-lora",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'


Here's the resources created associated with the lora custom resource.

.. figure:: ../assets/images/lora-service-discovery-resources.png
  :alt: lora-service-discovery-resources
  :width: 70%
  :align: center


1. A new Kubernetes service will be created with the exact same name as ModelAdapter name.

2. The ``podSelector`` is used to filter the matching pods. In this case, it will match pods with label ``model.aibrix.ai/name=qwen-coder-1-5b-instruct``. Make sure your base model have this label.
This ensures that the LoRA adapter is correctly associated with the right pods.

.. attention::

    Note: this is only working with vLLM engine. If you use other engine, feel free to open an issue.


Troubleshooting Phase Transitions
----------------------------------

If your ModelAdapter gets stuck in a particular phase, here are common issues and solutions:

**Stuck in Pending Phase:**

- Check if pods matching ``podSelector`` exist and are running
- Verify that pods have the correct labels (``model.aibrix.ai/name`` and ``adapter.model.aibrix.ai/enabled=true``)
- Ensure pods are in Ready state

**Stuck in Scheduled Phase:**

- This was a known issue in earlier versions that has been fixed
- Check controller logs for any loading errors: ``kubectl logs -n aibrix-system deployment/aibrix-controller-manager``
- Verify vLLM pods have ``VLLM_ALLOW_RUNTIME_LORA_UPDATING`` enabled

**Stuck in Loading Phase:**

- Check if the ``artifactURL`` is accessible and valid
- For Hugging Face models, ensure the model path exists
- Check authentication if using private repositories
- Review controller logs for specific loading errors
- The controller will retry up to 5 times with exponential backoff before switching to alternative pods

**Failed to Reach Running Phase:**

- Check if Kubernetes Service creation succeeded: ``kubectl get svc <adapter-name>``
- Verify EndpointSlice creation: ``kubectl get endpointslices``
- Check pod readiness and health status

**Useful Commands for Debugging:**

.. code-block:: bash

    # Check ModelAdapter status
    kubectl describe modeladapter <adapter-name>
    
    # Check controller logs
    kubectl logs -n aibrix-system deployment/aibrix-controller-manager
    
    # Check pod logs
    kubectl logs <pod-name>
    
    # Check services and endpoints
    kubectl get svc,endpointslices -l model.aibrix.ai/name=<adapter-name>

Reliability Features
--------------------

AIBrix ModelAdapter includes several advanced reliability features to ensure robust LoRA adapter deployments in production environments:

Automatic Retry Mechanism
^^^^^^^^^^^^^^^^^^^^^^^^^^

The controller implements an intelligent retry system for adapter loading:

- **Up to 5 retry attempts** per pod with exponential backoff
- **Base interval of 5 seconds**, doubling on each retry (5s, 10s, 20s, 40s, 80s)
- **Intelligent error detection** for retriable vs. non-retriable errors
- **Graceful handling** of common startup errors like "connection refused"

.. code-block:: yaml

    # Retry behavior is automatic - no configuration needed
    # Controller tracks retry state via annotations:
    # - adapter.model.aibrix.ai/retry-count.<pod-name>
    # - adapter.model.aibrix.ai/last-retry-time.<pod-name>

Pod Switching and Failover
^^^^^^^^^^^^^^^^^^^^^^^^^^^

When adapter loading fails on initial pods after maximum retries:

- **Automatic pod selection** of alternative healthy pods
- **Maintains desired replica count** by switching to available pods
- **Preserves scheduler preferences** (least-adapters, default) for new selections
- **Ensures eventual consistency** - adapters will load on healthy pods

Pod Health Validation
^^^^^^^^^^^^^^^^^^^^^^

The controller implements comprehensive pod health checks:

- **Readiness validation**: Pods must be in Ready state before selection
- **Stability requirements**: Pods must be stable (ready for 5+ seconds) to avoid flapping
- **Continuous monitoring**: Unhealthy pods are automatically removed from instances list
- **Startup tolerance**: Handles pod startup delays gracefully

Connection Error Handling
^^^^^^^^^^^^^^^^^^^^^^^^^^

Robust error handling for various network and startup conditions:

.. code-block:: text

    Retriable Errors (will trigger retry):
    - "connection refused" 
    - "connection reset"
    - "timeout"
    - "no route to host"
    - "network is unreachable" 
    - "temporary failure"
    - "service unavailable"
    - "bad gateway"

    Non-Retriable Errors (will fail immediately):
    - Authentication errors
    - Invalid model format
    - Configuration errors

Advanced Configuration Examples
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

**High-Availability Setup:**

Here's a production-ready configuration demonstrating reliability features:

.. literalinclude:: ../../../samples/adapter/adapter-reliability-demo.yaml
   :language: yaml

**Multi-Replica Distribution:**

For load balancing across multiple pods:

.. literalinclude:: ../../../samples/adapter/adapter-multi-replica.yaml
   :language: yaml
   :lines: 1-31

**Testing Commands:**

.. code-block:: bash

    # 1. Apply the reliability demo adapter:
    kubectl apply -f samples/adapter/adapter-reliability-demo.yaml
    
    # 2. Monitor phase transitions:
    watch kubectl describe modeladapter reliability-demo-lora
    
    # 3. Check controller logs for retry behavior:
    kubectl logs -n aibrix-system deployment/aibrix-controller-manager -f
    
    # 4. Test pod failure scenario (delete a pod to trigger switching):
    kubectl delete pod <pod-name>
    # Watch how controller handles pod loss and reschedules
    
    # 5. Verify final state:
    kubectl get modeladapter reliability-demo-lora -o yaml

Monitoring and Observability
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

**Phase Monitoring:**

The reliability features are transparent through the standard ModelAdapter status:

.. code-block:: bash

    # Monitor real-time phase transitions
    watch kubectl describe modeladapter <adapter-name>
    
    # Check for retry annotations (during loading failures)
    kubectl get modeladapter <adapter-name> -o yaml | grep -A 5 annotations

**Controller Logs:**

Monitor retry behavior and pod switching in controller logs:

.. code-block:: bash

    # View retry attempts and pod switching
    kubectl logs -n aibrix-system deployment/aibrix-controller-manager -f | grep -E "(retry|switching|pod)"
    
    # Example log entries:
    # I0830 03:55:42.221143 1 modeladapter_controller.go:557] "Selected pods for adapter scheduling" ModelAdapter="default/my-lora" selectedPods=["pod-1"]
    # I0830 03:55:45.331456 1 modeladapter_controller.go:987] "Retriable error loading adapter" pod="pod-1" error="connection refused"
    # I0830 03:55:50.445789 1 modeladapter_controller.go:1120] "Successfully loaded adapter on pod" pod="pod-2" ModelAdapter="default/my-lora"

**Events and Conditions:**

Reliability events are recorded in the ModelAdapter status conditions:

.. code-block:: bash

    kubectl describe modeladapter <adapter-name>
    
    # Example events showing retry and recovery:
    Events:
      Type    Reason                 Message
      ----    ------                 -------
      Normal  Scheduled              ModelAdapter has selected 1 pods for scheduling: [pod-1]
      Warning MaxRetriesExceeded     Max retries exceeded for pod pod-1: connection refused
      Normal  Scheduled              ModelAdapter has selected 1 pods for scheduling: [pod-2]
      Normal  Loaded                 Successfully loaded adapter on pod pod-2

Best Practices for Production
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

1. **Deploy Multiple Base Model Replicas:**

   .. code-block:: yaml
   
       # Ensure multiple pods are available for failover
       spec:
         replicas: 3  # At least 3 pods for reliability

2. **Use Multiple Adapter Replicas:**

   .. code-block:: yaml
   
       # Distribute adapter across multiple pods
       spec:
         replicas: 2  # Load adapter on 2 different pods

3. **Configure Appropriate Health Checks:**

   .. code-block:: yaml
   
       # In base model deployment
       livenessProbe:
         httpGet:
           path: /health
           port: 8000
         initialDelaySeconds: 60
         periodSeconds: 30
         failureThreshold: 3

4. **Monitor Adapter Health:**

   .. code-block:: bash
   
       # Regular health monitoring
       kubectl get modeladapters -o wide
       kubectl describe modeladapter <name>
       
       # Set up alerts on phase transitions
       kubectl get events --field-selector involvedObject.kind=ModelAdapter

More configurations
-------------------

Model Registry
^^^^^^^^^^^^^^

Currently, we support Huggingface model registry, S3 compatible storage and local file system.

1. If your model is hosted on Huggingface, you can use the ``artifactURL`` with ``huggingface://`` prefix to specify the model url. vLLM will download the model from Huggingface and load it into the pod in runtime.

2. If you put your model in S3 compatible storage, you have to use AIBrix AI Runtime at the same time. You can use the ``artifactURL`` with ``s3://`` prefix to specify the model url. AIBrix AI Runtime will download the model from S3 on the pod and load it with ``local model path`` in vLLM.

3. If you use shared storage like NFS, you can use the ``artifactURL`` with ``/`` absolute path to specify the model url (``/models/yard1/llama-2-7b-sql-lora-test`` as an example). It's users's responsibility to make sure the model is mounted to the pod.


Model api-key Authentication
^^^^^^^^^^^^^^^^^^^^^^^^^^^^

User may pass in the argument ``--api-key`` or environment variable ``VLLM_API_KEY`` to enable the server to check for API key in the header.

.. code-block:: bash

    python3 -m vllm.entrypoints.openai.api_server --api-key sk-kFJ12nKsFakefVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BFake

We already have an example and you can ``kubectl apply -f samples/adapter/adapter-with-key.yaml``.


In that case, lora model adapter can not query the vLLM server correctly, showing ``{"error":"Unauthorized"}`` error. You need to update ``additionalConfig`` field to pass in the API key.

.. literalinclude:: ../../../samples/adapter/adapter-api-key.yaml
   :language: yaml


You need to send the request with ``--header 'Authorization: Bearer your-api-key'``

.. code-block:: bash

    # send request to base model
    curl -v http://${ENDPOINT}/v1/completions \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer sk-kFJ12nKsFakefVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BFake" \
        -d '{
            "model": "qwen-code-lora-with-key",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'


Runtime Support Sidecar
^^^^^^^^^^^^^^^^^^^^^^^

Starting from v0.2.0, controller manager by default will talk to runtime sidecar to register the lora first and then the runtime sidecar will sync with inference engine to finish the eventual registration.
This is used to build the abstraction between controller manager and inference engine. If you like to directly sync with vLLM to load the loras, you can update the controller-manager ``kubectl edit deployment aibrix-controller-manager -n aibrix-system``
and remove the ``

.. code-block:: yaml
  :emphasize-lines: 7

    spec:
      containers:
      - args:
        - --leader-elect
        - --health-probe-bind-address=:8081
        - --metrics-bind-address=0
        - --enable-runtime-sidecar # this line should be removed
