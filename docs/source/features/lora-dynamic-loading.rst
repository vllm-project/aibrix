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

If you run ```kubectl describe modeladapter qwen-code-lora``, you will see the status of the lora adapter.

.. code-block:: bash

    $ kubectl describe modeladapter qwen-code-lora
    .....
    Status:
      Conditions:
        Last Transition Time:  2025-02-16T19:14:50Z
        Message:               Starting reconciliation
        Reason:                ModelAdapterPending
        Status:                Unknown
        Type:                  Initialized
        Last Transition Time:  2025-02-16T19:14:50Z
        Message:               ModelAdapter default/qwen-code-lora has been allocated to pod default/qwen-coder-1-5b-instruct-5587f4c57d-kml6s
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
    Events:   <none>

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
