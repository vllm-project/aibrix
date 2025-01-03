
```shell
kubectl apply -f docs/development/app/metadata.yaml
kubectl -n aibrix-system port-forward svc/aibrix-metadata-service 8090:8090 &
```

# Create user
```shell
curl http://localhost:8090/CreateUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name","rpm": 100,"tpm": 1000}'
```

# Read user
```shell
curl http://localhost:8090/ReadUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name"}'
```

# Update user
```shell
curl http://localhost:8090/UpdateUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name","rpm": 1000,"tpm": 10000}'
```

# Delete user
```shell
curl http://localhost:8090/DeleteUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name"}'
```