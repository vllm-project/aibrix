.. _kvcache-offloading:

==================
KVCache Offloading
==================

.. note::
    If you’re not yet familiar with the concepts of KVCache L1 and L2, please refer to AIBrix KVCache Offloading document at :ref:`aibrix_kvcache-offloading-framework`.

.. note::
    The AIBrix KVCache Offloading framework can be used as a standalone component — there’s no need to install the entire AIBrix stack.

.. note::
    Since v0.4.0, both vLLM V0 and V1 connectors are supported.

.. note::
    Since v0.5.0, both vLLM and `SGLang <https://github.com/sgl-project/sglang/tree/main/python/sglang/srt/mem_cache/storage/aibrix_kvcache>`_ are supported.

.. warning::
    Currently, only FlashAttention and XFormers are supported.

.. _l1_cache_example:

L1 Cache Example
----------------

.. note::
    We use a customized version of vLLM integrated with *AIBrix Offloading Connectors* to showcase the usage.


Before deploying the inference engine, please use ``kubectl get pods -n aibrix-system`` and ``kubectl get pods -n envoy-gateway-system`` to ensure ``envoy-gateway`` and ``aibrix-gateway`` are running. Other components are optional.

.. code-block:: console
  :emphasize-lines: 5,13,14

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


Now let's use the following yaml to create an engine deployment:

.. literalinclude:: ../../../samples/kvcache/l1cache/vllm.yaml
   :language: yaml
   :linenos:
   :emphasize-lines: 87,88,90,91,92,93,94,95,96,97,98,99,100,101


.. code-block:: console

    $ kubectl apply -f samples/kvcache/l1cache/vllm.yaml

    deployment.apps/deepseek-r1-distill-llama-8b created
    service/deepseek-r1-distill-llama-8b created


.. note::
    * Right now, the recommended connector for vLLM v0.10.2 is ``AIBrixOffloadingConnectorV1Type3``. You can switch to other AIBrix connectors if needed by specifying the ``kv_connector`` parameter of ``--kv-transfer-config``.
    * If you prefer to use vLLM V0, please set ``VLLM_USE_V1`` to ``0`` and change the value of ``--kv-transfer-config`` from ``'{"kv_connector":"AIBrixOffloadingConnectorV1Type3", "kv_role":"kv_both"}'`` to ``'{"kv_connector":"AIBrixOffloadingConnector", "kv_role":"kv_both"}'``
    * ``AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB`` needs to choose a proper value based on the pod memory resource requirement. For instance, if the pod memory resource requirement is ``P`` GB and the estimated memory consumption of the inference engine is ``E`` GB, we can set ``AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB`` to ``P / tensor-parallel-size - E``.

Now let's use ``kubectl get pods`` command to ensure the inference service is running:

.. code-block:: console

    $ kubectl get pods -w

    NAME                                            READY   STATUS            RESTARTS   AGE
    deepseek-r1-distill-llama-8b-6bb7c97459-lhh77   0/1     PodInitializing   0          87s
    deepseek-r1-distill-llama-8b-6bb7c97459-lhh77   1/1     Running           0          4m44s


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
    < date: Wed, 21 May 2025 00:52:06 GMT
    < server: uvicorn
    < content-type: application/json
    < target-pod: 192.168.3.22:8000
    < request-id: 34a19ba1-88f2-4aa0-b914-5a28609d6b0a
    < transfer-encoding: chunked
    <
    {"id":"chatcmpl-4ae8be13-5bbf-4bc0-92b6-6e8814296c57","object":"chat.completion","created":1747788726,"model":"deepseek-r1-distill-llama-8b","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":null,"content":"Okay, so I need to create a container called \"vllm-openai\" using Docker. I'm a bit new to this, so I'll have to figure it out step by step. Let me start by understanding what a Docker container is. From what I know, a container is a lightweight virtualization layer that allows me to package and run applications in isolated environments called containers. Docker makes this process easier by managing the containers and their images.\n\nI want to create a container that's specifically for running OpenAI's VLLM (Voss-LSTM), which is an open-source implementation of the original VLLM model by OpenAI. So, the container should have everything necessary to run this model, including the required dependencies and the model itself.\n\nFirst, I'll need to get the OpenAI VLLM code. I think it's available on GitHub, so I'll clone the repository. Let me check the URL: it's probably something like https://github.com/openai/vllm-cpp. Once I have the code, I need to build it. The instructions likely mention using CMake for the build process. I'll have to make sure I have CMake installed on my system. If not, I'll need to install it using my package manager.\n\nAfter cloning and building the code, I need to create a Docker image. The Dockerfile will specify the base image, which should be something like Ubuntu 20.04 LTS since it's a common and supported version. I'll need to set the working directory and copy the built VLLM files into the container. Also, I should install any system dependencies that the VLLM might need, like libraries or tools.\n\nI remember that VLLM requires certain Python packages, so I'll need to install those inside the container. The requirements.txt file probably lists all the necessary packages. Using pip to install them within the container makes sense. Additionally, since the model is quite large, the container might need more memory and CPU resources. I'll set up a non-root user for better security practices and ensure the permissions are set correctly so that the container can run the model without issues.\n\nI also need to expose the necessary ports. The VLLM server might run on port 8080, so I'll map that port in the Docker setup. For testing, I can use curl or a web interface to send requests to this port and see if the model responds correctly.\n\nLet me outline the steps I'll take:\n\n1. Clone the VLLM repository.\n2. Build the VLLM using CMake.\n3. Create a Dockerfile that includes the base OS, build tools, system dependencies, and copies the built VLLM files.\n4. Install the required Python packages using pip.\n5. Set up the container with proper user permissions and resource limits.\n6. Build and run the container.\n7. Test the container by sending requests to the exposed port.\n\nI'm a bit unsure about some parts. For example, how to handle the build process in the Dockerfile? I think the Dockerfile will need to have the necessary CMake commands and possibly install build dependencies like build-essential. Also, I need to make sure that the container has enough memory allocated to run the VLLM model, which can be quite resource-intensive.\n\nAnother thing I'm not sure about is the user setup. Why do I need a non-root user? Isn't it easier to run everything as root? Well, running as a non-root user is more secure, especially since Docker containers have root privileges by default. So, I should create a user and switch to it before running the model.\n\nI should also think about how the container handles persistence. Since the VLLM model is built outside the container, the container will only have the necessary files. If I need to persist the model, I'll have to copy it into the container during the build process. Otherwise, each container restart will require rebuilding the model, which might be time-consuming.\n\nLet me think about the Dockerfile structure. It should start with a FROM instruction based on an Ubuntu image. Then, set the working directory, install build-essential and cmake, clone the repository, build it, and then copy the built files into the container. After that, I'll switch to a non-root user and install the Python dependencies.\n\nWait, but the VLLM requires certain libraries like TensorFlow? Or is it self-contained? I think the VLLM is a standalone model, so maybe it doesn't rely on external libraries beyond what's already in the build. But I should check the requirements to be sure.\n\nAlso, the model is quite large, so the container might take up a lot of disk space. I should consider using a larger disk or use a persistent volume if I need to keep the model data.\n\nI should also document the container, maybe add some notes on how to use it, like the exposed ports and any required environment variables. For example, the VLLM might need an API key or specific configurations to run.\n\nTesting is important. After building the container, I can run it and use curl to send a request to the exposed port. If the response is as expected, the container is working. If not, I'll have to troubleshoot, maybe checking the logs or ensuring all dependencies are correctly installed.\n\nI'm a bit worried about performance. The VLLM model is designed for research purposes, so it's going to be computationally heavy. I should set resource limits in the Docker run command to prevent it from using too much of the host system's resources.\n\nIn summary, the process involves setting up the build environment, compiling the VLLM code into a Docker image, installing necessary dependencies, and ensuring the container runs securely and efficiently.\n</think>\n\nTo create a Docker container for OpenAI's VLLM, follow these organized steps:\n\n### Step-by-Step Guide\n\n1. **Clone the VLLM Repository**\n   - Clone the VLLM repository from GitHub:\n     ```bash\n     git clone https://github.com/openai/vllm-cpp.git\n     ```\n   - Navigate to the cloned directory:\n     ```bash\n     cd vllm-cpp\n     ```\n\n2. **Build the VLLM**\n   - Ensure you have CMake installed. If not, install it using your package manager.\n   - Build the VLLM using CMake:\n     ```bash\n     mkdir build\n     cd build\n     cmake ..\n     make\n     ```\n\n3. **Create the Dockerfile**\n   - Open a new file named `Dockerfile` and insert the following content:\n     ```dockerfile\n     FROM ubuntu:20.04\n\n     WORKDIR /app\n\n     # Install build tools\n     RUN apt-get update && apt-get install -y build-essential cmake\n     # Install system dependencies\n     RUN apt-get install -y libboost-system-dev libboost-filesystem-dev \\\n       libboost-chrono-dev libboost-serialization-dev libboost-headers\n     # Copy the built VLLM files\n     COPY build/vllm-cpp .\n     # Install Python dependencies\n     RUN useradd -m vllmuser && chown -R vllmuser:vllmuser .\n     RUN pip install -r requirements.txt\n     # Switch to non-root user\n     USER vllmuser\n     ```\n   - **Note:** Replace `requirements.txt` with your actual file path or content if you haven't created one yet.\n\n4. **Build and Run the Container**\n   - Build the Docker image:\n     ```bash\n     docker build -t vllm-openai .\n     ```\n   - Run the container, allocating enough resources (e.g., 4GB RAM and 4 CPUs):\n     ```bash\n     docker run -d --name vllm-openai \\\n       -e \"HTTP_PROXY=http://proxy.example.com:8080\" \\\n       -e \"HTTPS_PROXY=http://proxy.example.com:8080\" \\\n       --ulimits cgroup:1 --cpu-shares 1 --memory 4g \\\n       vllm-openai\n     ```\n   - Replace `proxy.example.com` with your actual proxy server if needed.\n\n5. **Test the Container**\n   - Check if the container is running:\n     ```bash\n     docker ps\n     ```\n   - Use `curl` to test the API:\n     ```bash\n     curl http://localhost:8080\n     ```\n   - If the response is as expected, the container is functioning correctly.\n\n### Notes\n\n- **User Permissions:** The container uses a non-root user (`vllmuser`) for security reasons.\n- **Dependencies:** Ensure all system a* Connection #0 to host localhost left intact
    nd Python dependencies are correctly installed as per the VLLM requirements.\n- **Resources:** Adjust CPU and memory allocations based on your system's capacity to handle the VLLM's computational demands.\n- **Volumes:** Consider using a persistent volume to store the VLLM model for longer-term use.\n\nBy following these steps, you'll have a containerized version of OpenAI's VLLM ready to run, ensuring security, efficiency, and ease of use.","tool_calls":[]},"logprobs":null,"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":12,"total_tokens":1887,"completion_tokens":1875,"prompt_tokens_details":null},"prompt_logprobs":null}


.. _l2_cache_example:

L2 Cache Example
----------------


let's deploy the distributed KV cache cluster with the following yaml configuration:

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
    * Right now, the recommended connector for vLLM v0.10.2 is ``AIBrixOffloadingConnectorV1Type3``. You can switch to other AIBrix connectors if needed by specifying the ``kv_connector`` parameter of ``--kv-transfer-config``.
    * If you prefer to use vLLM V0, please set ``VLLM_USE_V1`` to ``0`` and change the value of ``--kv-transfer-config`` from ``'{"kv_connector":"AIBrixOffloadingConnectorV1Type3", "kv_role":"kv_both"}'`` to ``'{"kv_connector":"AIBrixOffloadingConnector", "kv_role":"kv_both"}'``
    * In this example, we set ``AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED=0`` to explicitly disable ``L1Cache`` and use ``L2Cache`` only.
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

Profiling Example
-----------------

To profile the AIBrix Offloading Connectors, before launching the inference engines, you need to start the profiling service with the following sample YAML file:

.. literalinclude:: ../../../samples/kvcache/profiling/profiling_svc.yaml
   :language: yaml
   :linenos:

.. code-block:: console

    $ kubectl apply -f samples/kvcache/profiling/profiling_svc.yaml

.. code-block:: console

    $ kubectl get svc
    NAME                       TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)    AGE
    aibrix-kvcache-profiling   ClusterIP   10.70.106.217   <none>        4040/TCP   80m

Once the profiling service is running, let's set up port forwarding so that we can browse the profiling results and flamegraphs from local:

* Run ``kubectl port-forward svc/aibrix-kvcache-profiling 4040:4040 &`` to set up port forwarding

Now let's launch the inference engine with the following environment variables set in engine's YAML file (please refer to :ref:`l1_cache_example` and :ref:`l2_cache_example` for more details).

.. code-block:: yaml

    env:
      - name: AIBRIX_KV_CACHE_OL_PROFILING_ENABLED
        value: "1"
      - name: AIBRIX_KV_CACHE_OL_PROFILING_SERVER_ADDRESS
        value: "http://aibrix-kvcache-profiling:4040"

Run a task to generate requests to the inference engine, and you can browse the profiling results and flamegraphs from local after a while:

* Open `http://localhost:4040` in your browser
* Adjust the query parameters as illustrated in the following figure to show the flamegraph of AIBrix Offloading Connectors

.. figure:: ../assets/images/aibrix-kvcache-profiling.png
  :alt: aibrix-kvcache-profiling
  :width: 99%
  :align: center
