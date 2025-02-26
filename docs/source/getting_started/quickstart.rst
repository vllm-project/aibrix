.. _quickstart:

==========
Quickstart
==========

Install AIBrix
^^^^^^^^^^^^^^

Install Docker and Kubernetes, get your Kubernetes cluster ready, and run the following commands to install AIBrix components in your Kubernetes cluster.

.. note::
    If you just want to install specific components or specific version, please check installation guidance for more installation options.

.. code-block:: bash

    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.2.0/aibrix-dependency-v0.2.0.yaml
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.2.0/aibrix-core-v0.2.0.yaml

Wait for few minutes and run `kubectl get pods -n aibrix-system` to check pod status util they are ready.

.. code-block:: bash

    NAME                                         READY   STATUS    RESTARTS   AGE
    aibrix-controller-manager-56576666d6-gsl8s   1/1     Running   0          5h24m
    aibrix-gateway-plugins-c6cb7545-r4xwj        1/1     Running   0          5h24m
    aibrix-gpu-optimizer-89b9d9895-t8wnq         1/1     Running   0          5h24m
    aibrix-kuberay-operator-6dcf94b49f-l4522     1/1     Running   0          5h24m
    aibrix-metadata-service-6b4d44d5bd-h5g2r     1/1     Running   0          5h24m
    aibrix-redis-master-84769768cb-fsq45         1/1     Running   0          5h24m


Deploy Base Model
^^^^^^^^^^^^^^^^^

.. note::
In the given example, please ensure that your node is equipped with NVIDIA GPUs. Otherwise, the pod will stuck in pending state.

Save yaml as `model.yaml` and run `kubectl apply -f model.yaml`.

.. literalinclude:: ../../../samples/quickstart/model.yaml
   :language: yaml

Ensure that:

1. The `Service` name matches the `model.aibrix.ai/name` label value in the `Deployment`.
2. The `--served-model-name` argument value in the `Deployment` command is also consistent with the `Service` name and `model.aibrix.ai/name` label.


Invoke the Model Endpoint via Gateway API
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Depending on where you deployed the AIBrix, you can use either of the following options to query the gateway.

.. code-block:: bash

    # Option 1: Kubernetes cluster with LoadBalancer support
    LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
    ENDPOINT="${LB_IP}:80"

    # Option 2: Dev environment without LoadBalancer support. Use port forwarding way instead
    kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &
    ENDPOINT="localhost:8888"

.. attention::

    Some cloud provider like AWS EKS expose the endpoint at hostname field, if that case, you should use ``.status.loadBalancer.ingress[0].hostname`` instead.


.. code-block:: bash

    # completion api
    curl -v http://${ENDPOINT}/v1/completions \
        -H "Content-Type: application/json" \
        -d '{
            "model": "deepseek-r1-distill-llama-8b",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
        }'

    # chat completion api
    curl http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "help me write a random generator in python"}
        ]
    }'

If you meet problems exposing external IPs, feel free to debug with following commands. `101.18.0.4` is the ip of the gateway service.

.. code-block:: bash

    kubectl get svc -n envoy-gateway-system
    NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)                                   AGE
    envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.96.239.246   101.18.0.4    80:32079/TCP                              10d
    envoy-gateway                            ClusterIP      10.96.166.226   <none>        18000/TCP,18001/TCP,18002/TCP,19001/TCP   10d
