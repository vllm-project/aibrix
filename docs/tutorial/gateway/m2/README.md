# Deploy test model M1

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllmm2:v0.1.0 -f Dockerfile .
kind load docker-image aibrix/vllmm2:v0.1.0
```

2. Deploy mocked model image
```shell
kubectl apply -f deployment.yaml
kubectl port-forward svc/m2 16000:16000 &
```

## Test python app separately

```shell
curl http://localhost:16000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "m2",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```