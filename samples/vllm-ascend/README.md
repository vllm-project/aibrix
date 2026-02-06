# Deploying AIBrix with vLLM-ascend

### Supporting version

| Component | Version Requirement                            |
| :--- |:-----------------------------------------------|
| **MindCluster** | `> 7.3.0`                                      |
| **Kubernetes (k8s)** | `>= 1.25.x`                                    |
| **Container Runtime** | **containerd** >= 1.7.x OR **Docker** >= 24.x  |
| **AiBrix** | `>= 0.5.0`                                       |

### Environment Preparation
Make sure you have already installed aibrix: [Quickstart](https://aibrix.readthedocs.io/latest/getting_started/quickstart.html)

Deploy MindCluster: [MindCluster Deployment Guide](https://www.hiascend.com/document/detail/zh/mindcluster/72rc1/deployer/deployerug/deployer_0001.html)

Download image: [vllm-ascend:v0.11.0rc1-a3](https://quay.io/repository/ascend/vllm-ascend?tab=tags&tag=v0.11.0rc1-a3)

The `job.yaml` configuration uses several `hostPath` volumes. Ensure the following specific directories and files exist on the cluster nodes before deployment to avoid mounting failures:

* **Directory**: `/user/restore/reset/`
* **File**: `/user/restore/reset/default.reset-config-default-qwen25`

### Model Deployment
Apply yaml

```
kubectl apply -f job.yaml
```

### Service Invocation
Invoke the model endpoint using gateway API: [AIBrix Quickstart](http://aibrix.readthedocs.io/latest/getting_started/quickstart.html#invoke-the-model-endpoint-using-gateway-api)

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



