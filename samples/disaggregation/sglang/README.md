# SGLang P/D Disaggregation Examples

Note: 
- The examples in this directory are for demonstration purposes only. Feel free to use your own model path, rdma network, image etc instead.
- Routing solution in the examples uses SGlang upstream router `kvcache-container-image-hb2-cn-beijing.cr.volces.com/aibrix/sglang:community_v0.4.9.post1-patch` images. AIBrix Envoy Routing examples will be updated shortly. 
- SGLang images are built with mooncake-transfer-engine, if you want to use nixl, please build your own images.
- AIBrix storm service support replica mode and pool mode, please refer to [AIBrix storm service]( mode, please refer to [AIBrix storm service](https://aibrix.readthedocs.io/latest/designs/aibrix-storm-service.html) for more details.
- `vke.volcengine.com/rdma`, `k8s.volcengine.com/pod-networks` and `NCCL_IB_GID_INDEX` are specific to Volcano Engine Cloud. Feel free to customize it for your own cluster.


## Configuration

Check following configurations.

### RBAC required in SGlang Router

Please apply the following RBAC rules in your cluster and make sure sglang-router uses `default` service account.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pod-read
  namespace: default
rules:
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
  - watch
  - list
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: pod-read-binding
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: pod-read
subjects:
- kind: ServiceAccount
  name: default
  namespace: default
```

### Run Query Example inside the Router 

```bash
curl http://localhost:30000/v1/chat/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "/models/Qwen2.5-7B-Instruct",
    "messages": [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "help me write a random generator in python"}
    ]
}'
```
