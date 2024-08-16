# Install backed storage for persist rpm/tpm configuration
kubectl apply -f redis.yaml

# Add rpm/tpm config 
kubectl exec -it redis-master-<pod-name> -- redis-cli

set aibrix:<user-name>_TPM_LIMIT 100
set aibrix:<user-name>_RPM_LIMIT 10

# Install extension proc
make build && make apply

# Request based on model name
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: varun" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

# restart envoy gateway after applying envoy patch policy
```shell
kubectl apply -f docs/development/app/gateway.yaml 
k describe envoypatchpolicy epp
egctl config envoy-proxy route

kubectl rollout restart deployment envoy-gateway -n envoy-gateway-system
```

# Request based on specific pod
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "target-pod: 10.244.1.3:8000" \
  -H "user: varun" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```