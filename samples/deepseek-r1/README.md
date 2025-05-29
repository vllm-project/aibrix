# Deepseek-r1 671B multi-host Deployment in AIBrix

In this demo, we will show how to deploy a production-grade Deepseek-r1 671B model using AIBrix.
The deployment will utilize the full weights (671B) of the model without any quantization, showcasing AIBrix's capabilities in handling large-scale language models for inference services. 

## Prerequisite

Before the AIBrix deployment, some preliminary work such as model weight download to object storage or share file system, and customized container image setup has been completed.
We don't cover all the step but just address the critical part instead.

### Cluster

Deepseek-r1-671B requires 16 80GB GPUs. We use following instance specs for the testing. You can definitely similar setups based on your environments.

- instance: ecs.ebmhpcpni3l.48xlarge * 2
- CPU: 192vCPU
- Memory: 2048GiB DRAM
- GPU: NVIDIA H20-SXM5-96GB*8
- Network: 400 Gbps * 8 RDMA + 96 Gbps
- Disk: Local NVME 3576GiB * 4

### vLLM Image

The image used for this deployment is `aibrix/vllm-openai:v0.7.3.self.post1`, which is a custom-built image by AIBrix. There are two main reasons behind it:
- In the upstream v0.7.3, there was an issue with the NCCL version that sometimes caused the system to hang([#vllm/issues/13136](https://github.com/vllm-project/vllm/issues/13136)). We addressed this by upgrading nvidia-nccl-cu12==2.25.1 to enhance the stability of the communication.
- In the Ray dependencies of v0.7.3, a regression problem occurred where our previous modifications in vLLM were overwritten. To mitigate this issue, we reintroduced `ray[default,adag]`. 

You can definitely build the image by yourself using following Dockerfile.

```dockerfile
FROM vllm/vllm-openai:v0.7.3
RUN pip3 install -U ray[default,adag]==2.40.0
RUN pip3 install -U nvidia-nccl-cu12
ENTRYPOINT [""]
```

> Note: For users in China, you may prefix the image name with `aibrix-container-registry-cn-beijing.cr.volces.com/` when pulling the image from our registry.
> For instance, instead of just `aibrix/vllm-openai:v0.7.3.self.post1`, you should use `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/vllm-openai:v0.7.3.self.post1`. The same rule applies to `aibrix/runtime:v0.3.0`.

### Model weights

Users can select different storage options for the [model weights](https://huggingface.co/deepseek-ai/DeepSeek-R1)  according to their cloud service providers. Here, we will discuss three common scenarios:
- **HuggingFace**: A pod can directly retrieve model weights from HuggingFace. However, it's important to note that fetching from HuggingFace is not recommended. This is because the varying tensor sizes result in numerous random reads, which significantly reduces network and I/O efficiency.
- **Persistent Volume**: Cloud providers such as AWS with Lustre or Google Cloud offer persistent disks through their Container Storage Interface (CSI). Users can effortlessly mount a Persistent Volume Claim (PVC) to the pod, enabling seamless access to the model weights stored on these persistent disks.
- **Object Storage with AI Runtime**: Users have the option to store model weights in object storage services like Amazon S3 or Google Cloud Storage (GCS). In this case, the AIbrix AI runtime will automatically download the model to the host volume. This approach offers flexibility and scalability, leveraging the advantages of object storage for storing large amounts of data.
- **Local Disk**: For local disk storage, an additional process is required to download the model weights to the local disk. We assume that a local volume is available and can be successfully mounted to the pod. This option may be suitable for environments where local storage offers performance benefits or when there are specific security or latency requirements.

| Storage Options                                 | Description                              | Sample Files                           |
| ----------------------------------------------- |------------------------------------------|----------------------------------------|
| HuggingFace                                     | no volume needed                         | [Link](./deepseek-r1-huggingface.yaml) |
| Persistent Volume                               | models volume, PVC                       | [Link](./deepseek-r1-pvc.yaml)         |
| Object Storage(S3 / GCS) with AIBrix AI Runtime | models volume, HostPath                  | [Link](./deepseek-r1-ai-runtime.yaml)  |
| Local Disk                                      | models volume, HostPath + InitContainer  | [Link](./deepseek-r1-local-nvme.yaml)  |

### High Performance Network

To leverage RDMA to achieve the best performance in network communication, we need to configure the pod configuration to use RDMA.
`k8s.volcengine.com/pod-networks` is configured in the annotation like below and `vke.volcengine.com/rdma: "8"` is needed at the pod resource level.
This is just one example on volcano engine cloud, You need to make corresponding change based on your own cloud environment.

```yaml
  k8s.volcengine.com/pod-networks: |
    [
      {
        "cniConf":{
            "name":"rdma"
        }
      },
      ....
      {
        "cniConf":{
            "name":"rdma"
        }
      }
    ]
```

Besides that, we also need `IPC_LOCK` and Share Memory Support.

```yaml
  securityContext:
    capabilities:
      add:
      - IPC_LOCK
```


## Installation

You need AIBrix v0.2.1, which has been proven effective for our multi-nodes deployment needs. When deploying AIBrix, it's important to note that the AIBrix mirror is mainly hosted on Dockerhub,
and deploying it in environments has Dockerhub access limitation can be challenging. To overcome this, let check our tutorial to override the control plane images with your own registry, enabling a smooth deployment of customized AIBrix.

It should be emphasized that there might be aspects related to the cloud environment, for example, `ReadWriteMany` volume provider, `Local Disk` etc. 
We take [Volcano Cloud](https://www.volcengine.com/) as a reference here, but users are advised to check their own cloud infrastructure.

While we can provide some general recommendations, due to limited resources, we haven't been able to test all cloud platforms thoroughly.
We encourage the community to contribute by submitting a Pull Request (PR) to help improve our support for different clouds.

```
kubectl create -f https://github.com/vllm-project/aibrix/releases/download/v0.3.0/aibrix-dependency-v0.3.0.yaml
kubectl create -f https://github.com/vllm-project/aibrix/releases/download/v0.3.0/aibrix-core-v0.3.0.yaml
```

## How AIBrix support deepseek-r1

AIBrix plays a crucial role in supporting the Deepseek-r1 671B model deployment. It provides a comprehensive platform that enables distributed orchestration, efficient traffic routing, and intelligent scaling capabilities.
These features are essential for handling the large-scale and resource-intensive nature of the Deepseek-r1 671B model.

We will briefly talk about `RayClusterFleet`, `Gateway Plugin` and `Autoscaler` related capabilities for this case before we jump into the deployment. 

![deepseek-r1](./static/deepseek-deployment.png)

`RayClusterFleet`  plays a pivotal role in managing the distributed inference orchestration. It provisions pods and constructs a Ray cluster, within which the vLLM server is launched. Each mini Ray cluster thus constitutes an inference replica.
In a multi-node environment, the vLLM HTTP server is initiated on the head node. The remaining GPU nodes function as workers, with no HTTP service running on them. Correspondingly, the AIBrix router routes requests **exclusively** to the head node. Similarly, the autoscaler fetches metrics **solely** from the service pod.
This distributed configuration ensures that the orchestration, routing, and autoscaling mechanisms operate effectively. By managing multi-node setups in a manner analogous to single-node operations, it streamlines the overall deployment process for super large models like deepseek-r1.

## Model Deployment

Firstly, make sure you change network and object storage configuration, for example using [s3](https://aibrix.readthedocs.io/latest/features/runtime.html#download-from-s3).
`DOWNLOADER_ALLOW_FILE_SUFFIX` has to be `json, safetensors, py` for deepseek-r1. 

Then run following command to deploy the model and associated kv cache based autoscaling strategy. Note, it really depends on the network speed between compute and object storage, the deployment make takes up to 20 mins.

```bash
kubectl apply -f deepseek-r1-ai-runtime.yaml
kubectl apply -f deepseek-r1-autoscaling.yaml
```

After a while, you should see running pods similar to the ones shown below.

```bash
kubectl get pods
NAME                                                          READY   STATUS              RESTARTS          AGE
deepseek-r1-671b-7ffb754f75-ggnzf-head-7xr6q                  1/1     Running             0                 25m
deepseek-r1-671b-7ffb754f75-ggnzf-worker-group-worker-gj456   1/1     Running             0                 25m
```

## Send requests

Expose endpoint through following command. 

```bash
# Option 1: Kubernetes cluster with LoadBalancer support
LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
ENDPOINT="${LB_IP}:80"

# Option 2: Dev environment without LoadBalancer support. Use port forwarding way instead
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &
ENDPOINT="localhost:8888"
```

```bash
curl http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" -H "routing-strategy: least-request" \
    -d '{
        "model": "deepseek-r1-671b",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "Who won the world series in 2020?"}
        ]
    }'
```
> Note: `-H "routing-strategy: least-request"` header can be removed if you like to use default kubernetes routing strategies

You supposed to see some response like below

```bash
{"id":"chatcmpl-d26583d2-96e5-42c4-a322-133c7d0e505d","object":"chat.completion","created":1740967604,"model":"deepseek-r1-671b","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":null,"content":"<think>\nOkay, the user is asking which team won the World Series in 2020. Let me recall, the World Series is the championship series of Major League Baseball (MLB) in the United States. I remember that 2020 was a unique year because of the COVID-19 pandemic, which affected the schedule and format of the season. The season was shortened, and there were some changes to the playoff structure.\n\nI think the Los Angeles Dodgers won the World Series around that time. Let me verify. The 2020 World Series was held at a neutral site, which was Globe Life Field in Arlington, Texas, to minimize travel and reduce the risk of COVID-19 spread. The Dodgers faced the Tampa Bay Rays. The Dodgers were led by players like Mookie Betts, Corey Seager, and Clayton Kershaw. They won the series in six games. The clinching game was Game 6, where the Dodgers beat the Rays 3-1. That victory gave the Dodgers their first title since 1988, ending a long drought.\n\nWait, let me make sure I got the opponent right. Was it the Rays or another team? Yes, I'm pretty confident it was the Rays because earlier in the playoffs, teams like the Braves and Dodgers were in the National League, while the Rays were the American League champions. The Rays had a strong team with players like Randy Arozarena, who had a standout postseason. But the Dodgers ultimately triumphed. So the answer should be the Los Angeles Dodgers. Let me double-check a reliable source if I'm unsure. Confirming now... yes, the Dodgers won the 2020 World Series against the Tampa Bay Rays in six games. So the user needs to know both the winner and maybe a bit of context, like it being in a neutral location. Okay, ready to provide a concise answer with those details.\n</think>\n\nThe Los Angeles Dodgers won the 2020 World Series, defeating the Tampa Bay Rays in six games. This championship marked the Dodgers' first title since 1988. Notably, the 2020 series was held at Globe Life Field in Arlington, Texas—a neutral site—due to COVID-19 health and safety protocols.","tool_calls":[]},"logprobs":null,"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":19,"total_tokens":472,"completion_tokens":453,"prompt_tokens_details":null},"prompt_logprobs":null}%
```

## Monitoring

We assume you have [Prometheus](https://prometheus.io/) setup in the cluster, then you can deploy `ServiceMonitor` to allow it to fetch metrics from deepseek deployment.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: deepseek-r1-svc-discover
  namespace: default
  labels:
    volcengine.vmp: "true"
spec:
  endpoints:
  - port: service
  namespaceSelector:
    matchNames:
    - default
  selector:
    matchLabels:
      ray.io/node-type: head
```

you can use our own built [dashboard](./static/AIBrix%20Engine%20Dashboard%20(vLLM)-1741078999667.json) to visit your model performance.

![dashboard](static/deepseek-dashboard.png)

> Note: After the dashboard is imported in Grafana, you may need minor changes like `labels` due to different prometheus setup.

## FAQ

### How to check model download progress?

Get your pod list first and then check `init-model` container logs.

```bash
NAME                                                          READY   STATUS              RESTARTS         AGE
deepseek-r1-671b-6ffbdd5f5c-5klq4-head-b9pwj                  0/1     Init:0/1            0                10m
deepseek-r1-671b-6ffbdd5f5c-5klq4-worker-group-worker-986g5   0/1     Init:0/2            0                10m
```

```bash
kubectl logs -f deepseek-r1-671b-6ffbdd5f5c-5klq4-head-b9pwj -c init-model
......
2025-03-09 08:02:17 UTC - utils.py:87 - need_to_download - INFO - File model-00101-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:23 UTC - utils.py:87 - need_to_download - INFO - File model-00102-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:30 UTC - utils.py:87 - need_to_download - INFO - File model-00103-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:36 UTC - utils.py:87 - need_to_download - INFO - File model-00104-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:42 UTC - utils.py:87 - need_to_download - INFO - File model-00105-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:48 UTC - utils.py:87 - need_to_download - INFO - File model-00106-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:02:55 UTC - utils.py:87 - need_to_download - INFO - File model-00107-of-000163.safetensors not exist in local, start to download...
2025-03-09 08:03:01 UTC - utils.py:87 - need_to_download - INFO - File model-00108-of-000163.safetensors not exist in local, start to download...
```

### How to monitor the autoscaling?

We can use `kubectl describe podautoscaler deepseek-r1-671b-autoscaling` to check detailed behavior.

```bash
Status:
  Actual Scale:  1
  Conditions:
    Last Transition Time:  2025-03-04T03:48:21Z
    Message:               the KPA controller was able to get the target's current scale
    Reason:                SucceededGetScale
    Status:                True
    Type:                  AbleToScale
Events:
  Type    Reason        Age                     From           Message
  ----    ------        ----                    ----           -------
  Normal  AlgorithmRun  19m (x1615 over 4h49m)  PodAutoscaler  KPA algorithm run. currentReplicas: 1, desiredReplicas: 1, rescale: false
  Normal  AlgorithmRun  2m21s (x92 over 17m)    PodAutoscaler  KPA algorithm run. currentReplicas: 1, desiredReplicas: 1, rescale: false
```
