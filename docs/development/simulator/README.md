## vLLM application simulator

1. Build simulated base model image
```dockerfile
docker build -t aibrix/vllm-simulator:nightly -f Dockerfile .

# If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
# Therefore, the following command is not necessary.
kind load docker-image aibrix/vllm-simulator:nightly
```

2. Deploy simulated model image
```shell
kubectl apply -f docs/development/simulator/deployment.yaml
kubectl -n aibrix-system port-forward svc/simulator-llama2-7b 8000:8000 1>/dev/null 2>&1 &
```

## Test python app separately

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

```shell
kubectl delete -f docs/development/simulator/deployment.yaml
```