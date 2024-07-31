## Installation <a name = "Installation"></a>

Install envoy gateway and setup HTTP Route
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

Port forward to the Envoy service:
```
kubectl -n envoy-gateway-system port-forward service/${ENVOY_SERVICE} 8888:80 &
```

Setup model m1 and m2, following their README.md
```
/docs/tutorial/gateway/m1/README.md
/docs/tutorial/gateway/m2/README.md
```


Run requests
```
Success
curl http://localhost:8888/v1/chat/completions \
  -H "model: m1" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "m1",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'

Error
curl http://localhost:8888/v1/chat/completions \
  -H "model: m2" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "m1",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'

Success
curl http://localhost:8888/v1/chat/completions \
  -H "model: m2" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "m2",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```


delete envoy gateway and corresponding objects
```
kubectl delete -f quickstart.yaml

kubectl delete -f m1/deployment.yaml

kubectl delete -f m2/deployment.yaml

helm uninstall eg -n envoy-gateway-system
```