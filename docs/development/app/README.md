## Mocked vLLM application

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm:v0.1.0 -f Dockerfile .
kind load docker-image aibrix/vllm:v0.1.0
```

2. Deploy mocked model image
```shell
kubectl apply -f deployment.yaml
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
```
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.1.0 -n envoy-gateway-system --create-namespace

kubectl wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

kubectl apply -f docs/development/app/gateway.yaml
```

Check stauts
```
helm status eg -n envoy-gateway-system

helm get all eg -n envoy-gateway-system
```

Port forward to the Envoy service:
```
kubectl -n envoy-gateway-system port-forward service/envoy-default-eg-e41e7b31 8888:80 &
```

Start model router controller
```
make run
```

Test request (ensure header model name matches with deployment's model name for routing)
```shell
curl http://localhost:8888/v1/chat/completions \
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
```
kubectl delete -f gateway.yaml

helm uninstall eg -n envoy-gateway-system
```