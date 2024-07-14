
```shell
apiVersion: apps/v1
kind: Deployment
metadata:
  name: deepseek-33b-instruct
  namespace: default
  labels:
    model.aibrix.ai: deepseek-33b-instruct
spec:
  replicas: 1
  selector:
    matchLabels:
      model.aibrix.ai: deepseek-33b-instruct
  template:
    metadata:
      labels:
        model.aibrix.ai: deepseek-33b-instruct
    spec:
      containers:
      - name: deepseek-33b-instruct
        image: your-docker-registry/deepseek-33b-instruct:latest
        resources:
          requests:
            nvidia.com/gpu: "2"  # Assuming you need a GPU
          limits:
            nvidia.com/gpu: "2"
        ports:
        - containerPort: 8080
        env:
        - name: MODEL_PATH
          value: "/models/deepseek-33b-instruct"
        volumeMounts:
        - name: model-storage
          mountPath: /models
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
```


```shell
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelAdapter
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"model.aibrix.ai/v1alpha1","kind":"ModelAdapter","metadata":{"annotations":{},"name":"text2sql-lora-1","namespace":"default"},"spec":{"additionalConfig":{"model-artifact":"jeffwan/rank-1"},"baseModel":"llama2-70b","podSelector":{"matchLabels":{"model.aibrix.ai":"llama2-70b"}},"schedulerName":"default-model-adapter-scheduler"}}
  creationTimestamp: "2024-07-14T21:09:18Z"
  generation: 2
  name: text2sql-lora-1
  namespace: default
  resourceVersion: "788513"
  uid: 61fd3d3c-8549-4742-8f43-7df8c66f0a6d
spec:
  additionalConfig:
    model-artifact: jeffwan/rank-1
  baseModel: llama2-70b
  podSelector:
    matchLabels:
      model.aibrix.ai: llama2-70b
  schedulerName: default-model-adapter-scheduler
status:
  phase: Configuring
```

```shell
apiVersion: v1
kind: Service
metadata:
  creationTimestamp: "2024-07-14T21:42:57Z"
  labels:
    model.aibrix.ai/base-model: llama2-70b
    model.aibrix.ai/model-adapter: text2sql-lora-1
  name: text2sql-lora-1
  namespace: default
  ownerReferences:
  - apiVersion: model.aibrix.ai/v1alpha1
    blockOwnerDeletion: true
    controller: true
    kind: ModelAdapter
    name: text2sql-lora-1
    uid: 61fd3d3c-8549-4742-8f43-7df8c66f0a6d
  resourceVersion: "789949"
  uid: bef1fb3e-27d2-4663-ac87-14ef721c3693
spec:
  clusterIP: None
  clusterIPs:
  - None
  internalTrafficPolicy: Cluster
  ipFamilies:
  - IPv4
  ipFamilyPolicy: SingleStack
  ports:
  - name: http
    port: 8000
    protocol: TCP
    targetPort: 8000
  publishNotReadyAddresses: true
  selector:
    model.aibrix.ai: llama2-70b
  sessionAffinity: None
  type: ClusterIP
status:
  loadBalancer: {}
```


```shell
addressType: IPv4
apiVersion: discovery.k8s.io/v1
endpoints:
- addresses:
  - 10.1.2.133
  conditions: {}
kind: EndpointSlice
metadata:
  creationTimestamp: "2024-07-14T21:42:59Z"
  generation: 1
  labels:
    kubernetes.io/service-name: text2sql-lora-1
  name: text2sql-lora-1
  namespace: default
  ownerReferences:
  - apiVersion: model.aibrix.ai/v1alpha1
    blockOwnerDeletion: true
    controller: true
    kind: ModelAdapter
    name: text2sql-lora-1
    uid: 61fd3d3c-8549-4742-8f43-7df8c66f0a6d
  resourceVersion: "789958"
  uid: bf913402-b97d-426d-89a9-8ea734ba8a7a
ports:
- name: http
  port: 80
  protocol: TCP
```


problem here. 2nd was created by endpoint.
```shell
text2sql-lora-1                     IPv4          80                           10.1.2.133     2m24s
text2sql-lora-1-hzdl9               IPv4          8000                         10.1.2.133     2m26s
```

```shell
apiVersion: v1
kind: Endpoints
metadata:
  annotations:
    endpoints.kubernetes.io/last-change-trigger-time: "2024-07-14T21:42:57Z"
  creationTimestamp: "2024-07-14T21:42:57Z"
  labels:
    model.aibrix.ai/base-model: llama2-70b
    model.aibrix.ai/model-adapter: text2sql-lora-1
    service.kubernetes.io/headless: ""
  name: text2sql-lora-1
  namespace: default
  resourceVersion: "789951"
  uid: 7f64255c-ff58-49fa-9ec3-f19164f884ba
subsets:
- addresses:
  - ip: 10.1.2.133
    nodeName: docker-desktop
    targetRef:
      kind: Pod
      name: lora-test
      namespace: default
      uid: 408484b6-38e9-4fa1-8b2c-e57753a0f220
  ports:
  - name: http
    port: 8000
    protocol: TCP
```