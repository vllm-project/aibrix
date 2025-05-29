.. _kvcache-offloading:

====================
KVCache Offloading
====================

.. warning::
    Currently, only FlashAttention and XFormers are supported.

The growing demands of modern models and increasing context lengths in LLM inference have led to KV caches consuming progressively more GPU memory, pushing against the hardware limits of even the most advanced GPUs. Recent systems like `Dynamo <https://github.com/ai-dynamo/dynamo>`_, `LMCache <https://github.com/LMCache/LMCache>`_, and `MoonCake <https://github.com/kvcache-ai/Mooncake>`_ have developed solutions that offload KV cache to external memory hierarchies, spanning from CPU memory to SSDs. :ref:`kvcache-offloading-framework` supports the offloading of KV cache to CPU memory as well by only enabling its DRAM-backed ``L1Cache``. While this approach does not enable KV cache sharing across multiple engines, it eliminates the complexity of distributed KV cache setup and configuration. More importantly, by leveraging the significantly larger capacity of CPU memory, this method delivers substantial performance gains —- making it an ideal solution for use cases that prioritize scalable KV cache capacity over cross-engine KV reuse.

Example
-------

.. note::
    We use an internal version of vLLM integrated with *AIBrix Offloading Connector* to showcase the usage.


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
    ``AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB`` needs to choose a proper value based on the pod memory resource requirement. For instance, if the pod memory resource requirement is ``P`` GB and the estimated memory consumption of the inference engine is ``E`` GB, we can set ``AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB`` to ``P / tensor-parallel-size - E``.

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
