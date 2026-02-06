# Deploying AIBrix with vLLM-ascend

### Supporting version

| Component | Version Requirement                            |
| :--- |:-----------------------------------------------|
| **MindCluster** | `> 7.3.0`                                      |
| **Kubernetes (k8s)** | `>= 1.25.x`                                    |
| **Container Runtime** | **containerd** >= 1.7.x OR **Docker** >= 24.x  |
| **AiBrix** | `>= 0.5.0`                                       |

### Environment Preparation
Make sure you have already installed aibrix:https://aibrix.readthedocs.io/latest/getting_started/quickstart.html

Deploy MindCluster:https://www.hiascend.com/document/detail/zh/mindcluster/72rc1/deployer/deployerug/deployer_0001.html

Download image:https://quay.io/repository/ascend/vllm-ascend?tab=tags&tag=latest

### Model Deployment
Apply yaml

```
kubectl apply -f job.yaml
```

### Service Invocation
Invoke the model endpoint using gateway API
```
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &
ENDPOINT="localhost:8888"
```

completion api
```
curl -v http://${ENDPOINT}/v1/completions \
    -H "Content-Type: application/json" \
    -H "routing-strategy: random" \
    -d '{
        "model": "Qwen-2.5-0.5B",
        "prompt": "San Francisco is a",
        "max_tokens":128,
        "temperature":0
    }'
```



