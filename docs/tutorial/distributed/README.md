# Run vLLM Distributed Inference with Ray

## Environment Setup

### Configure the GPU Cloud Instance

> Note: This tutorial assumes you are using LambdaLabs. If you are using other large cloud providers, you can skip this step and move to the next section.

Run the following commands to install and verify the GPU runtimes for the container:

```bash
./install.sh # Install nvkind and GPU runtimes
./verify.sh # Verify GPU runtime is working
```

```
nvkind cluster create --config-template=nvkind-single-node.yaml
```

### Install the Kubernetes add-ons

Install Kubernetes components:

```
./setup.sh
```

## Launch vLLM instance using RayJob

Apply the Kubernetes job configuration to launch the vLLM instance:
```
kubectl apply -f job.yaml
```

To request the model, find the head service name and make the request. You can either port-forward the port or use a load balancer based on your requirements.

Example request using curl:

```
curl http://vllm-server-raycluster-kf6cq-head-svc.default.svc.cluster.local:8000/v1/completions \
-H "Content-Type: application/json" \
-d '{
"model": "facebook/opt-13b",
"prompt": "San Francisco is a",
"max_tokens": 128,
"temperature": 0
}'
```

This README should guide you through the process of setting up and running vLLM distributed inference with Ray on a GPU cloud instance. If you encounter any issues, please consult the respective documentation for the tools and services used.
