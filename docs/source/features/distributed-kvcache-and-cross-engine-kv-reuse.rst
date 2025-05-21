.. _dist-kv-and-cross-engine-kv-reuse:

=============================================
Distributed KVCache and Cross-Engine KV Reuse
=============================================

.. warning::
    Currently, only FlashAttention and XFormers are supported.

The growing demand for large language models has significantly increased the need for expansive KV cache capacity. While CPU memory offloading effectively addresses moderate scaling needs, production environments handling massive-scale, dynamic workloads require even greater scalability -- particularly when memory needs exceed single-node capacities. To address this, AIBrix enables distributed KV cache services as its ``L2Cache`` backends, which can scale horizontally across multiple nodes to meet capacity demands.

In the meantime, as LLM deployments scale across multiple engines in the cluster, the redundancy of KV caches across engines introduces substantial inefficiencies. Repeated computations of common prompt prefixes waste GPU cycles and HBM bandwidth. AIBrix solves this challenge by enabling efficient **cross-engine KV reuse** through a high-performance, shared distributed KV cache, optimizing resource utilization at scale.

.. figure:: ../assets/images/aibrix-infinistore-arch-overview.png
  :alt: aibrix-infinistore-arch-overview
  :width: 100%
  :align: center

Example
-------

.. note::
    We use an internal version of vLLM integrated with *AIBrix Offloading Connector* to showcase the usage.


Before deploying the inference engine, please use ``kubectl get pods -n aibrix-system`` and ``kubectl get pods -n envoy-gateway-system`` to ensure orchestration components are running.

.. code-block:: console

    $ kubectl get pods -n aibrix-system

    NAME                                         READY   STATUS    RESTARTS   AGE
    aibrix-controller-manager-586dd9f868-465dz   1/1     Running   0          16h
    aibrix-gateway-plugins-5fcbcbfc84-h7qc8      1/1     Running   0          16h
    aibrix-gpu-optimizer-66f49fd947-gfncm        1/1     Running   0          4d23h
    aibrix-kuberay-operator-55f4d4d666-bd7hj     1/1     Running   0          4d23h
    aibrix-metadata-service-6d5cc8ddd6-444mb     1/1     Running   0          4d23h
    aibrix-redis-master-c9b4967c5-pdnkg          1/1     Running   0          16h

    $ kubectl get pods -n envoy-gateway-system
    NAME                                                     READY   STATUS    RESTARTS   AGE
    envoy-aibrix-system-aibrix-eg-903790dc-fd69b467d-6fg2z   2/2     Running   0          16h
    envoy-gateway-5d48549b5c-6r4cd                           1/1     Running   0          16h

Once orchestration components are ready, let's deploy the distributed KV cache cluster with the following yaml configuration:

.. literalinclude:: ../../../samples/kvcache/infinistore/kvcache.yaml
   :language: yaml
   :linenos:
   :emphasize-lines: 45

.. note::
    We have changed L45 from one replica to three replicas in this example, thus we will find three KV cache pods running in the cluster.

.. code-block:: console

    $ kubectl apply -f samples/kvcache/infinistore/kvcache.yaml

    kvcache.orchestration.aibrix.ai/kvcache-cluster created

.. code-block:: console

    $ kubectl get pods -w
    NAME                                  READY   STATUS              RESTARTS   AGE
    kvcache-cluster-0                     0/1     ContainerCreating   0          59s
    kvcache-cluster-kvcache-watcher-pod   1/1     Running             0          59s
    kvcache-cluster-redis                 1/1     Running             0          59s
    kvcache-cluster-0                     1/1     Running             0          2m43s
    kvcache-cluster-1                     0/1     Pending             0          0s
    kvcache-cluster-1                     0/1     Pending             0          0s
    kvcache-cluster-1                     0/1     ContainerCreating   0          0s
    kvcache-cluster-1                     0/1     ContainerCreating   0          2s
    kvcache-cluster-1                     1/1     Running             0          5s
    kvcache-cluster-2                     0/1     Pending             0          0s
    kvcache-cluster-2                     0/1     Pending             0          0s
    kvcache-cluster-2                     0/1     ContainerCreating   0          0s
    kvcache-cluster-2                     0/1     ContainerCreating   0          2s
    kvcache-cluster-2                     1/1     Running             0          4s

Now let's use the following yaml to create an engine deployment:

.. literalinclude:: ../../../samples/kvcache/infinistore/vllm.yaml
   :language: yaml
   :linenos:
   :emphasize-lines: 95,96,98,99,100,101,102,103,104,105,106,107,108,109,110,111,112,113,114,115,116,117,118,119,120


.. code-block:: console

    $ kubectl apply -f samples/kvcache/infinistore/vllm.yaml

    deployment.apps/deepseek-r1-distill-llama-8b created
    service/deepseek-r1-distill-llama-8b created


.. note::
    * In this example, we set ``AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED=0`` to explictly disable ``L1Cache`` and use ``L2Cache`` only.
    * Current version only supports using ``InfiniStore`` with RDMA transport. Please ensure ``AIBRIX_KV_CACHE_OL_INFINISTORE_CONNECTION_TYPE=RDMA`` is configured.
    * For InfiniBand, please set ``AIBRIX_KV_CACHE_OL_INFINISTORE_LINK_TYPE=IB``. For RoCE, please keep the default value ``Ethernet``.
    * ``AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST`` is used to configure which RDMA device can be used by the engine to access remote KV cache servers. For instance, if you allocate 8 GPUs for the engine pod and set ``AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST="mlx5_1,mlx5_2"``, then engine processes using GPU 0 to 3 will use ``mlx5_1``, and engine processes using GPU 4 to 7 will use ``mlx5_2``.
    * If GID indexes of RDMA devices are required in your environment, please append the GID index to each RDMA device (e.g., ``mlx5_1:6,mlx5_2:7``) in ``AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST``.
    * ``AIBRIX_KV_CACHE_OL_META_SERVICE_URL`` points to the Redis instance managing KV cache cluster metadata. In this example, it is set to ``redis://kvcache-cluster-redis:6379``, where ``kvcache-cluster`` is the KV cache deployment name.
    * ``AIBRIX_KV_CACHE_OL_META_SERVICE_BACKEND`` and ``AIBRIX_KV_CACHE_OL_META_SERVICE_CLUSTER_META_KEY`` are fixed in current version and should not be modified.

Now let's use ``kubectl get pods`` command to ensure the inference service is running:

.. code-block:: console

    $ kubectl get pods

    NAME                                            READY   STATUS    RESTARTS   AGE
    deepseek-r1-distill-llama-8b-6587db8894-pbbxk   1/1     Running   0          34s
    kvcache-cluster-0                               1/1     Running   0          7m55s
    kvcache-cluster-1                               1/1     Running   0          5m12s
    kvcache-cluster-2                               1/1     Running   0          5m7s
    kvcache-cluster-kvcache-watcher-pod             1/1     Running   0          7m55s
    kvcache-cluster-redis                           1/1     Running   0          7m55s


Once the inference service is running, let's set up port forwarding so that we can test the service from local:

* Run ``kubectl get svc -n envoy-gateway-system`` to get the name of the Envoy Gateway service.

.. code-block:: console

    $ kubectl get svc -n envoy-gateway-system

    NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)                                   AGE
    envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.97.198.203   115.190.25.67   80:32269/TCP                              5d3h
    envoy-gateway                            ClusterIP      10.97.57.193    <none>          18000/TCP,18001/TCP,18002/TCP,19001/TCP   5d3h

* Run ``kubectl -n envoy-gateway-system port-forward svc/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &`` to set up port forwarding

.. code-block:: console

    $ kubectl -n envoy-gateway-system port-forward svc/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &

    Forwarding from 127.0.0.1:8888 -> 10080
    Forwarding from [::1]:8888 -> 10080

Now, let's test the service:

.. code-block:: shell

    curl -v "http://localhost:8888/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer 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" \
      -d '{
         "model": "deepseek-r1-distill-llama-8b",
         "messages": [{"role": "user", "content": "Created container vllm-openai"}],
         "temperature": 0.7
       }'

and its output would be:

.. code-block:: RST

    *   Trying [::1]:8888...
    * Connected to localhost (::1) port 8888
    > POST /v1/chat/completions HTTP/1.1
    > Host: localhost:8888
    > User-Agent: curl/8.4.0
    > Accept: */*
    > Content-Type: application/json
    > Authorization: Bearer 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
    > Content-Length: 173
    >
    Handling connection for 8888
    < HTTP/1.1 200 OK
    < x-went-into-req-headers: true
    < date: Wed, 21 May 2025 05:44:04 GMT
    < server: uvicorn
    < content-type: application/json
    < target-pod: 192.168.3.28:8000
    < request-id: f9b291ae-fbce-4b63-bba5-c8f04d812cd0
    < transfer-encoding: chunked
    <
    {"id":"chatcmpl-dce54e48-3c47-4d08-8ff9-dcec429fd486","object":"chat.completion","created":1747806244,"model":"deepseek-r1-distill-llama-8b","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":null,"content":"Okay, so I'm trying to create a container for VLLM-OpenAI. I'm a bit new to this, so I need to figure out where to start. I know that VLLM stands for Very Large Language Model, and OpenAI has their own models like GPT-4 and others. But I'm not exactly sure how to create a container for it. \n\nFirst, I think I need to understand what a container is. From what I remember, containers are like lightweight virtual machines that you can use to package up an application and its dependencies. Docker is a popular tool for creating and managing these containers. So, I probably need to use Docker to create a container that runs VLLM-OpenAI.\n\nI should check if there's an official Docker image for VLLM-OpenAI. Maybe OpenAI provides one? If not, I might have to build one myself. Building from source code would mean I need access to the model's codebase, which I'm not sure about. I should look up if there's a public repository for VLLM-OpenAI.\n\nWait, I think OpenAI has released some of their models as open-source, but I'm not certain about VLLM specifically. I should search for \"VLLM-OpenAI Docker\" or \"VLLM-OpenAI container\" to see if someone has already created a container. Maybe there's a GitHub repository or a Docker Hub page with the image.\n\nIf I can't find an existing image, I'll have to create one myself. To do that, I need to know what dependencies the model requires. VLLM is based on LLMs, so it probably needs libraries like PyTorch or TensorFlow. Also, it might require specific versions of Python or other tools. I should look up the installation instructions for VLLM-OpenAI to identify the necessary dependencies.\n\nI'll need a Dockerfile. The Dockerfile will include a base image, install the dependencies, and copy the model's code. I should make sure to use the correct Python version, as some models might have compatibility issues. I'll also need a requirements.txt file to list all the necessary Python packages and their versions.\n\nOnce the Dockerfile is set up, I can build the container using Docker. The command would be something like `docker build -t vllm-openai .` where `vllm-openai` is the name of the container. After building, I can run it with `docker run -it vllm-openai`, which will start an interactive session.\n\nI should also consider how to manage the model once the container is running. Do I need to pass it a prompt through stdin? How does it handle outputs? I should look up the usage instructions for VLLM-OpenAI to know how to interact with it within the container.\n\nAnother thing to think about is resource usage. VLLM models are computationally intensive, so I need to make sure the container has enough resources allocated. This can be done when running the container with options like `--cpuset` or `--memory` if necessary.\n\nI'm a bit worried about the size of the model. VLLM might have a large embedding size, so the container might become quite large. I should check if there are optimized versions or ways to reduce the model size without losing too much performance.\n\nAlso, I should think about versioning. If I create a container, I should name it something that includes the version number, like `vllm-openai-1.0`. This way, I can easily update to newer versions by rebuilding the container.\n\nI wonder if there are any specific commands or tools needed to run VLLM-OpenAI in a container. Maybe I need to use a specific framework or tooling that's already included in the container. I should make sure I have all the necessary command-line tools installed before trying to run it.\n\nI should also test the container locally to see if it works. Maybe start with a simple prompt to see if the model responds. If it doesn't, I'll need to troubleshoot whether it's an issue with the container setup or the model configuration.\n\nIn summary, my steps would be:\n1. Search for existing V* Connection #0 to host localhost left intact
    LLM-OpenAI containers or Docker images.\n2. If none found, create a new Dockerfile and requirements.txt.\n3. Install necessary dependencies and copy the model code.\n4. Build and run the container using Docker.\n5. Test the container with a sample input.\n6. Adjust resources and configurations as needed.\n\nI might run into issues like dependency conflicts or missing packages, so I should be prepared to update versions or check the model's documentation for specific requirements. Also, understanding how the model expects inputs and outputs is crucial for effective use.\n\nI think I've got a basic plan. Now, I'll try to find the existing resources or proceed to set up the Dockerfile if necessary. Let me start by searching for VLLM-OpenAI Docker container on GitHub or Docker Hub to see if someone else has done this before.\n</think>\n\nTo create a container for VLLM-OpenAI, follow these organized steps:\n\n1. **Search for Existing Containers**:\n   - Check Docker Hub or GitHub for existing VLLM-OpenAI containers. If found, use them as they may already be configured.\n\n2. **Prepare Your Environment**:\n   - Ensure you have Docker installed on your system.\n\n3. **Set Up the Project Structure**:\n   - Create a directory for your project.\n   - Within it, create a `Dockerfile` and a `requirements.txt` file.\n\n4. **Dockerfile Setup**:\n   - Use a base image that matches your system's requirements (e.g., `python:3.9-slim`).\n   - Install necessary dependencies from `requirements.txt`.\n   - Copy the VLLM-OpenAI code into the container.\n\n5. **requirements.txt**:\n   - List all Python packages needed, including specific versions, such as `transformers` and `torch`.\n\n6. **Build the Container**:\n   - Use the command `docker build -t vllm-openai .` to build the container.\n\n7. **Run the Container**:\n   - Start the container with `docker run -it vllm-openai` for an interactive session.\n\n8. **Test the Container**:\n   - Issue a test command to ensure the model responds, e.g., `echo \"Hello, how are you?\" | docker run -it vllm-openai`.\n\n9. **Optimize Resources**:\n   - Adjust resource allocation with options like `--cpuset` or `--memory` to handle computational demands.\n\n10. **Versioning**:\n    - Name your container with versioning, such as `vllm-openai-1.0`.\n\n11. **Troubleshooting**:\n    - If issues arise, check for dependency conflicts or review the model's documentation for specific requirements.\n\n12. **Documentation and Usage**:\n    - Familiarize yourself with how VLLM-OpenAI expects inputs and outputs for effective utilization.\n\nBy following these steps, you can efficiently create and manage a container for VLLM-OpenAI, ensuring it runs smoothly within your environment.","tool_calls":[]},"logprobs":null,"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":12,"total_tokens":1505,"completion_tokens":1493,"prompt_tokens_details":null},"prompt_logprobs":null}
