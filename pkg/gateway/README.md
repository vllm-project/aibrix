

## Installation <a name = "Installation"></a>

```
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.1.0 -n envoy-gateway-system --create-namespace

kubectl wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

kubectl apply -f quickstart.yaml -n default
```

Check stauts
```
helm status eg -n envoy-gateway-system

helm get all eg -n envoy-gateway-system
```

Get docker image
```
docker pull ghcr.io/cncf/mistral:latest

kind load docker-image ghcr.io/cncf/mistral:latest
```

Port forward to the Envoy service:
```
kubectl -n envoy-gateway-system port-forward service/${ENVOY_SERVICE} 8888:80 &

kubectl port-forward service/ollama 11434:6000 &
```

Curl the example app through Envoy proxy:
```
curl --verbose --header "Host: www.example.com" http://localhost:8888/get

curl -X POST http://localhost:11434/api/generate -d '{
  "model": "mistral",
  "prompt": "what is capital of california"
 }'


curl -X POST http://localhost:8888/api/generate -d '{
  "model": "mistral",
  "prompt": "what is capital of california"
 }'
```