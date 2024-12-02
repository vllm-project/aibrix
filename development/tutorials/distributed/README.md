# Run vLLM Distributed Inference with Ray

## Container Image

> Note: some upstream work has not been merged yet. So we need to do some downstream changes

```
FROM vllm/vllm-openai:v0.6.2
RUN apt update && apt install -y wget # important for future healthcheck
RUN pip3 install ray[default] # important for future healthcheck
COPY utils.py /usr/local/lib/python3.12/dist-packages/vllm/executor/ray_utils.py
ENTRYPOINT [""]
```

> Note: copy uitls.py from upstream version and remove the placement group validation logic. See [#228](https://github.com/aibrix/aibrix/issues/228) for more details.
> Note: No need to downgrade ray to v2.10.0. Seem only ray-project/ray image has issues.

Container Image Combination which supports the distributed multi-host inference.
- aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/kuberay-operator:v1.2.1-patch
- aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/vllm-openai:v0.6.2-distributed

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
