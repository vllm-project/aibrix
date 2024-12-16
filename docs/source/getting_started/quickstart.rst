.. _quickstart:

==========
Quickstart
==========

Install AIBrix
^^^^^^^^^^^^^^

Get your kubernetes cluster ready, run following commands to install aibrix components in your cluster.

.. note::
    If you just want to install specific components or specific version, please check installation guidance for more installation options.

.. code-block:: bash

    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-dependency-v0.2.0-rc.1.yaml
    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-core-v0.2.0-rc.1.yaml

Wait for few minutes and run `kubectl get pods -n aibrix-system` to check pod status util they are ready.

.. code-block:: bash

    NAME                                         READY   STATUS    RESTARTS   AGE
    aibrix-controller-manager-56576666d6-gsl8s   1/1     Running   0          5h24m
    aibrix-gateway-plugins-c6cb7545-r4xwj        1/1     Running   0          5h24m
    aibrix-gpu-optimizer-89b9d9895-t8wnq         1/1     Running   0          5h24m
    aibrix-kuberay-operator-6dcf94b49f-l4522     1/1     Running   0          5h24m
    aibrix-metadata-service-6b4d44d5bd-h5g2r     1/1     Running   0          5h24m
    aibrix-redis-master-84769768cb-fsq45         1/1     Running   0          5h24m


Deploy base model
^^^^^^^^^^^^^^^^^

Save yaml as `deployment.yaml` and run `kubectl apply -f deployment.yaml`.

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      labels:
        # Note: The label value `model.aibrix.ai/name` here must match with the service name.
        model.aibrix.ai/name: qwen25-7b-Instruct
        model.aibrix.ai/port: "8000"
        adapter.model.aibrix.ai/enabled: true
      name: qwen25-7b-Instruct
      namespace: default
    spec:
      replicas: 1
      selector:
        matchLabels:
          model.aibrix.ai/name: qwen25-7b-Instruct
      strategy:
        rollingUpdate:
          maxSurge: 25%
          maxUnavailable: 25%
        type: RollingUpdate
      template:
        metadata:
          labels:
            model.aibrix.ai/name: qwen25-7b-Instruct
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
                - Qwen/Qwen2.5-7B-Instruct
                - --served-model-name
                # Note: The `--served-model-name` argument value must also match the Service name and the Deployment label `model.aibrix.ai/name`
                - qwen25-7b-Instruct
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
        model.aibrix.ai/name: qwen25-7b-Instruct
        prometheus-discovery: "true"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
      name: qwen25-7b-Instruct
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
        model.aibrix.ai/name: qwen25-7b-Instruct
      type: ClusterIP

.. note::

   Ensure that:

   1. The `Service` name matches the `model.aibrix.ai/name` label value in the `Deployment`.
   2. The `--served-model-name` argument value in the `Deployment` command is also consistent with the `Service` name and `model.aibrix.ai/name` label.



Invoke the model endpoint using gateway api
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

    # Setup port forwarding to query gateway from local environment
    kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc  8888:80 &

.. code-block:: bash

    # model name in the header is required for gateway which is used by httproute (described in previous section) to forward request to appropriate model service

    curl -v http://localhost:8888/v1/completions \
        -H "Content-Type: application/json" \
        -H "model: qwen25-7b-Instruct" \
        -d '{
            "model": "qwen25-7b-Instruct",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'
