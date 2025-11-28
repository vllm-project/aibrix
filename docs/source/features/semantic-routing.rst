========================================
Intelligent Semantic Routing
========================================

This guide demonstrates how to integrate vLLM Semantic Router with vLLM AIBrix to build an intelligent Mixture-of-Models (MoM) system. The integration brings **system-level intelligence** through AI-powered semantic understanding, automated reasoning capabilities, and intelligent caching mechanisms, enabling production-grade LLM inference with enhanced quality, security, and cost efficiency.

What is vLLM Semantic Router?
========================================

vLLM Semantic Router is an AI-powered intelligent routing system for efficient LLM inference. It operates as an Envoy External Processor that semantically routes OpenAI API-compatible requests to the most suitable backend model using advanced neural network technologies.

**Key Features:**

* **🧠 Intelligent Routing**: Powered by ModernBERT fine-tuned models for intelligent intent understanding, it analyzes context, intent, and complexity to route requests to the best LLM
* **🛡️ AI-Powered Security**: Advanced PII Detection and Prompt Guard to identify and block jailbreak attempts, ensuring secure and responsible AI interactions
* **⚡ Semantic Caching**: Intelligent similarity cache that stores semantic representations of prompts, dramatically reducing token usage and latency through smart content matching

Benefits of Integration
========================================

Integrating vLLM Semantic Router with AIBrix provides several advantages:

**1. Intelligent Request Routing**
   Semantic Router analyzes incoming requests and routes them to the most appropriate model based on content understanding, while AIBrix's gateway efficiently manages traffic distribution across model replicas.

**2. Enhanced Scalability**
   AIBrix's autoscaler works seamlessly with Semantic Router to dynamically adjust resources based on routing patterns and real-time demand.

**3. Cost Optimization**
   By combining Semantic Router's intelligent routing with AIBrix's heterogeneous serving capabilities, you can optimize GPU utilization and reduce infrastructure costs while maintaining SLO guarantees through per-token unit economics.

**4. Production-Ready Infrastructure**
   AIBrix provides enterprise-grade features like distributed KV cache, GPU failure detection, and unified runtime management, making it easier to deploy Semantic Router in production environments.

**5. Simplified Operations**
   The integration leverages Kubernetes-native patterns and Gateway API resources, providing a familiar operational model for DevOps teams.

About vLLM AIBrix
========================================

`vLLM AIBrix <https://github.com/vllm-project/aibrix>`_ is an open-source initiative designed to provide essential building blocks to construct scalable GenAI inference infrastructure. AIBrix delivers a cloud-native solution optimized for deploying, managing, and scaling large language model (LLM) inference, tailored specifically to enterprise needs.

Prerequisites
========================================

Before starting, ensure you have the installed AIBrix components.

Step 1: Deploy vLLM Semantic Router
========================================

Deploy the semantic router service with all required components:

.. code-block:: bash

   # Clone the semantic router repository
   git clone git@github.com:vllm-project/semantic-router.git && cd semantic-router

   # Deploy semantic router using Kustomize
   kubectl apply -k deploy/kubernetes/aibrix/semantic-router

   # Wait for deployment to be ready (this may take several minutes for model downloads)
   kubectl wait --for=condition=Available deployment/semantic-router -n vllm-semantic-router-system --timeout=600s

   # Verify deployment status
   kubectl get pods -n vllm-semantic-router-system

Step 2: Deploy Demo LLM
========================================

Create a demo LLM to serve as the backend for the semantic router:

.. code-block:: bash

   # Deploy demo LLM
   kubectl apply -f deploy/kubernetes/aibrix/aigw-resources/base-model.yaml

   kubectl wait --timeout=2m -n default deployment/vllm-llama3-8b-instruct --for=condition=Available

Step 3: Create Gateway API Resources
========================================

Create the necessary Gateway API resources for the envoy gateway:

.. code-block:: bash

   kubectl apply -f deploy/kubernetes/aibrix/aigw-resources/gwapi-resources.yaml

Testing the Deployment
========================================

Method 1: Port Forwarding (Recommended for Local Testing)
----------------------------------------------------------

Set up port forwarding to access the gateway locally:

.. code-block:: bash

   # Get the Envoy service name
   export ENVOY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
     --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
     -o jsonpath='{.items[0].metadata.name}')

   kubectl port-forward -n envoy-gateway-system svc/$ENVOY_SERVICE 8080:80

Send Test Requests
----------------------------------------------------------

Once the gateway is accessible, test the inference endpoint:

.. code-block:: bash

   # Test math domain chat completions endpoint
   curl -i -X POST http://localhost:8080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{
       "model": "MoM",
       "messages": [
         {"role": "user", "content": "What is the derivative of f(x) = x^3?"}
       ]
     }'

You will see the response from the demo LLM, and additional headers injected by the semantic router:

.. code-block:: http

   HTTP/1.1 200 OK
   server: fasthttp
   date: Thu, 06 Nov 2025 06:38:08 GMT
   content-type: application/json
   x-inference-pod: vllm-llama3-8b-instruct-984659dbb-gp5l9
   x-went-into-req-headers: true
   request-id: b46b6f7b-5645-470f-9868-0dd8b99a7163
   x-vsr-selected-category: math
   x-vsr-selected-reasoning: on
   x-vsr-selected-model: vllm-llama3-8b-instruct
   x-vsr-injected-system-prompt: true
   transfer-encoding: chunked

   {"id":"chatcmpl-f390a0c6-b38f-4a73-b019-9374a3c5d69b","created":1762411088,"model":"vllm-llama3-8b-instruct","usage":{"prompt_tokens":42,"completion_tokens":48,"total_tokens":90},"object":"chat.completion","do_remote_decode":false,"do_remote_prefill":false,"remote_block_ids":null,"remote_engine_id":"","remote_host":"","remote_port":0,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"I am your AI assistant, how can I help you today? To be or not to be that is the question. Alas, poor Yorick! I knew him, Horatio: A fellow of infinite jest Testing, testing 1,2,3"}}]}

Cleanup
========================================

To remove the entire deployment:

.. code-block:: bash

   # Remove Gateway API resources and Demo LLM
   kubectl delete -f deploy/kubernetes/aibrix/aigw-resources

   # Remove semantic router
   kubectl delete -k deploy/kubernetes/aibrix/semantic-router

   # Delete kind cluster
   kind delete cluster --name semantic-router-cluster

Next Steps
========================================

* Set up monitoring and observability
* Implement authentication and authorization
* Scale the semantic router deployment for production workloads
