
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
apiVersion: yourdomain.com/v1alpha1
kind: ModelAdapter
metadata:
  name: lora-finetune-revision-1
  namespace: default
spec:
  baseModel: "deepseek-33b-instruct"
  podSelector:
    matchLabels:
      model.aibrix.ai: deepseek-33b-instruct
  schedulerName: "default-model-adapter-scheduler"
```

```shell
apiVersion: v1
kind: Service
metadata:
name: lora-finetune-revision-1
namespace: default
labels:
  model.aibrix.ai: deepseek-33b-instruct
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    model.aibrix.ai: deepseek-33b-instruct
  ports:
  - protocol: TCP
    port: 8000
    targetPort: 8000
```


```shell
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: lora-finetune-revision-1
  labels:
    kubernetes.io/service-name: llama3-8b-lora1
addressType: IPv4
ports:
  - name: http
    appProtocol: http
    protocol: TCP
    port: 8000
endpoints:
  - addresses:
      - "10.0.0.1"
  - addresses:
      - "10.0.0.2"
```
