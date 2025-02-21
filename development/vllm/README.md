# vLLM CPU application

Documents lists commands to deploy vLLM cpu application for local development

## Deploy model
### Download facebook/opt-125m model locally
```shell
huggingface-cli download facebook/opt-125m
```

### Setup kind cluster
Update path for huggingface cache in kind config
```shell
kind create cluster --config=./development/vllm/kind-config.yaml
```
(Optional) Load container image to docker context

> Note: If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
> Therefore, the following command is not necessary. Only kind user need this step.

```shell
docker pull aibrix/vllm-cpu-env:macos
kind load docker-image aibrix/vllm-cpu-env:macos
```

Build aibrix runtime component
```shell
make docker-build-all
kind load docker-image aibrix/runtime:nightly
```

### Deploy model
```shell
kubectl create -k vllm/config

kubectl delete -k vllm/config
```

### Setup port forwarding

```shell
kubectl port-forward svc/facebook-opt-125m 8000:8000 &
```

### Inference request
```shell
curl -v http://localhost:8000/v1/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-key-1234567890" \
  -d '{
     "model": "facebook-opt-125m",
     "prompt": "Say this is a test",
     "temperature": 0.5,
     "max_tokens": 512
   }'
```


## Deploy aibrix gateway
### Setup components
```shell
make docker-build-all
kubectl create -k config/dependency
kubectl create -k config/default
```

### Setup port forwarding for envoy service
```shell
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &
```

### Inference request
```shell
curl -v POST "http://localhost:8888/v1/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-key-1234567890" \
  --data '{
    "model": "facebook-opt-125m",
    "prompt": "Once upon a time,",
    "max_tokens": 512,
    "temperature": 0.5
  }'
```