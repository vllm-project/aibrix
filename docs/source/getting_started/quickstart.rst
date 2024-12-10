.. _quickstart:

==========
Quickstart
==========

Install AIBrix
^^^^^^^^^^^^^^

.. note::
    If following way doesn't work for you, please check installation guidance for more installation options.

.. code-block:: bash

    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-dependency-v0.2.0-rc.1.yaml
    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-core-v0.2.0-rc.1.yaml


Deploy base model
^^^^^^^^^^^^^^^^^

Save yaml as `deployment.yaml` and run `kubectl apply -f deployment.yaml`.

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      labels:
        # Note: The label value `model.aibrix.ai/name` here must match with the service name.
        model.aibrix.ai/name: llama-2-7b-hf
        model.aibrix.ai/port: "8000"
        adapter.model.aibrix.ai/enabled: true
      name: llama-2-7b-hf
      namespace: default
    spec:
      replicas: 1
      selector:
        matchLabels:
          model.aibrix.ai/name: llama-2-7b-hf
      strategy:
        rollingUpdate:
          maxSurge: 25%
          maxUnavailable: 25%
        type: RollingUpdate
      template:
        metadata:
          labels:
            model.aibrix.ai/name: llama-2-7b-hf
        spec:
          containers:
            - command:
                - python3
                - -m
                - vllm.entrypoints.openai.api_server
                - --host
                - "0.0.0.0"
                - --port
                - "8000"
                - --model
                - meta-llama/Llama-2-7b-hf
                - --served-model-name
                # Note: The `--served-model-name` argument value must also match the Service name and the Deployment label `model.aibrix.ai/name`
                - llama-2-7b-hf
                - --trust-remote-code
                - --enable-lora
              env:
                - name: VLLM_ALLOW_RUNTIME_LORA_UPDATING
                  value: "true"
              image: aibrix/vllm-openai:v0.6.1.post2
              imagePullPolicy: Always
              livenessProbe:
                failureThreshold: 3
                httpGet:
                  path: /health
                  port: 8000
                  scheme: HTTP
                initialDelaySeconds: 90
                periodSeconds: 5
                successThreshold: 1
                timeoutSeconds: 1
              name: vllm-openai
              ports:
                - containerPort: 8000
                  protocol: TCP
              readinessProbe:
                failureThreshold: 3
                httpGet:
                  path: /health
                  port: 8000
                  scheme: HTTP
                initialDelaySeconds: 90
                periodSeconds: 5
                successThreshold: 1
                timeoutSeconds: 1
              resources:
                limits:
                  nvidia.com/gpu: "1"
                requests:
                  nvidia.com/gpu: "1"
              volumeMounts:
                - name: dshm
                  mountPath: /dev/shm
          volumes:
            - name: dshm
              emptyDir:
                medium: Memory
                sizeLimit: "4Gi"

Save yaml as `service.yaml` and run `kubectl apply -f service.yaml`.

.. code-block:: yaml

    apiVersion: v1
    kind: Service
    metadata:
      labels:
        # Note: The Service name must match the label value `model.aibrix.ai/name` in the Deployment
        model.aibrix.ai/name: llama-2-7b-hf
        prometheus-discovery: "true"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
      name: llama-2-7b-hf
      namespace: default
    spec:
      ports:
        - name: serve
          port: 8000
          protocol: TCP
          targetPort: 8000
        - name: http
          port: 8080
          protocol: TCP
          targetPort: 8080
      selector:
        model.aibrix.ai/name: llama-2-7b-hf
      type: ClusterIP

.. note::

   Ensure that:

   1. The `Service` name matches the `model.aibrix.ai/name` label value in the `Deployment`.
   2. The `--served-model-name` argument value in the `Deployment` command is also consistent with the `Service` name and `model.aibrix.ai/name` label.


Register a user to authenticate the gateway
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-gateway-users 8090:8090

.. code-block:: bash

    curl http://localhost:8090/CreateUser \
      -H "Content-Type: application/json" \
      -d '{"name": "test-user","rpm": 100,"tpm": 10000}'



Invoke the model endpoint using gateway api
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

    # Setup port forwarding to query gateway from local environment
    kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc  8888:80 &

.. code-block:: bash

    # model name in the header is required for gateway which is used by httproute (described in previous section) to forward request to appropriate model service

    curl -v http://localhost:8888/v1/completions \
        -H "Content-Type: application/json" \
        -H "user: test-user" \
        -H "model: meta-llama/Llama-2-7b-hf" \
        -d '{
            "model": "meta-llama/llama-2-7b-hf",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'
