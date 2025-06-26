.. _advanced-k8s-examples:

=========================
Advanced Kubernetes Examples
=========================

Introduction
------------
This documentation demonstrates advanced deployment patterns with AIBrix on Kubernetes, featuring persistent model caching and optimized resource configuration.

Persistent Volume Claim Optimization
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
The following example shows how to:
- Create isolated namespace with security policies
- Deploy vLLM model with PVC for HuggingFace cache
- Configure liveness/readiness probes
- Set up monitoring annotations

.. code-block:: yaml

    apiVersion: v1
    kind: Namespace
    metadata:
      labels:
        pod-security.kubernetes.io/enforce: privileged
      name: aibrix-system-llm
    
    ---
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      labels:
        model.aibrix.ai/name: deepseek-r1-distill-llama-8b
        model.aibrix.ai/port: "8000"
      name: deepseek-r1-distill-llama-8b
      namespace: aibrix-system-llm
    spec:
      replicas: 1
      selector:
        matchLabels:
          model.aibrix.ai/name: deepseek-r1-distill-llama-8b
      template:
        metadata:
          labels:
            model.aibrix.ai/name: deepseek-r1-distill-llama-8b
        spec:
          volumes:
            - name: cache-volume
              persistentVolumeClaim:
                claimName: deepseek-r1-distill-llama-8b
            - name: shm
              emptyDir:
                medium: Memory
                sizeLimit: "2Gi"
          containers:
            - command:
                - python3
                - -m
                - vllm.entrypoints.openai.api_server
                - --host
                - "0.0.0.0"
                - --port
                - "8000"
                - --uvicorn-log-level
                - warning
                - --model
                - deepseek-ai/DeepSeek-R1-Distill-Llama-8B
                - --served-model-name
                - deepseek-r1-distill-llama-8b
                - --max-model-len
                - "12288"
              image: vllm/vllm-openai:v0.9.0
              imagePullPolicy: IfNotPresent
              name: vllm-openai
              ports:
                - containerPort: 8000
                  protocol: TCP
              resources:
                limits:
                  cpu: "8"
                  memory: 20G
                  nvidia.com/gpu: "1"
                requests:
                  cpu: "2"
                  memory: 6G
                  nvidia.com/gpu: "1"
              volumeMounts:
                - mountPath: /root/.cache/huggingface
                  name: cache-volume
                - name: shm
                  mountPath: /dev/shm
              livenessProbe:
                httpGet:
                  path: /health
                  port: 8000
                  scheme: HTTP
                failureThreshold: 3
                periodSeconds: 5
                successThreshold: 1
                timeoutSeconds: 1
                initialDelaySeconds: 100
              readinessProbe:
                httpGet:
                  path: /health
                  port: 8000
                  scheme: HTTP
                failureThreshold: 5
                periodSeconds: 5
                successThreshold: 1
                timeoutSeconds: 1
                initialDelaySeconds: 100
    
    ---
    apiVersion: v1
    kind: Service
    metadata:
      labels:
        model.aibrix.ai/name: deepseek-r1-distill-llama-8b
        prometheus-discovery: "true"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
      name: deepseek-r1-distill-llama-8b
      namespace: aibrix-system-llm
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
        model.aibrix.ai/name: deepseek-r1-distill-llama-8b
      type: ClusterIP
    
    ---
    apiVersion: v1
    kind: PersistentVolumeClaim
    metadata:
      name: deepseek-r1-distill-llama-8b
      namespace: aibrix-system-llm
    spec:
      accessModes:
        - ReadWriteOnce
      resources:
        requests:
          storage: 30Gi
      volumeMode: Filesystem

Key Features
------------
1. **PVC Caching** - 30Gi persistent storage for HuggingFace models at `/root/.cache/huggingface`
2. **Security Context** - Dedicated namespace with privileged security policy
3. **Resource Limits** - Explicit GPU/CPU/Memory allocations
4. **Health Monitoring** - Multi-stage probes with failure thresholds
5. **Metrics Integration** - Prometheus annotations for service discovery

Usage Notes
-----------
- Model name labels must match across Deployment/Service
- PVC storage class should match cluster configuration
- Adjust `max-model-len` according to actual model requirements
- Probe delays accommodate model loading time (100s initial delay)