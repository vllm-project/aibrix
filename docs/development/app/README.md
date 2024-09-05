## Mocked vLLM application

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm:v0.1.0 -f Dockerfile .
kind load docker-image aibrix/vllm:v0.1.0
```

2. Deploy mocked model image
```shell
kubectl apply -f docs/development/app/deployment.yaml
kubectl port-forward svc/llama2-70b 8000:8000 &
```

## Test python app separately

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```



## Test with envoy gateway

Install envoy gateway and setup HTTP Route
```shell
make docker-build && make docker-build-plugins

kind load docker-image aibrix/plugins:v0.1.0

make install && make deploy
```

Check stauts
```shell
helm status eg -n envoy-gateway-system

helm get all eg -n envoy-gateway-system
```

Port forward to the Envoy service:
```shell
kubectl -n aibrix-system port-forward service/envoy-aibrix-system-eg-95d1b654 8888:80 &
```

# Add rpm/tpm config 
```shell
kubectl -n aibrix-system exec -it aibrix-redis-master-<pod-name> -- redis-cli

set aibrix:your-user-name_TPM_LIMIT 100
set aibrix:your-user-name_RPM_LIMIT 10
```

Test request (ensure header model name matches with deployment's model name for routing)
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: your-user-name" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```


Delete envoy gateway and corresponding objects
```shell
make undeploy && make uninstall
```