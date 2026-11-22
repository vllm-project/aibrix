# ModelWarmup samples

`ModelWarmup` runs a finite, node-pinned Job for every selected node. A sample
can preload images, run custom containers, or combine both. The original
[`modelwarmup.yaml`](modelwarmup.yaml) remains the v1-compatible image-only
example.

## Prerequisites

Install the AIBrix ModelWarmup CRD and controller, then authorize a namespace
and at least one node in the same latency pool:

```sh
kubectl label namespace default resource-pool.aibrix.ai/name=latency-pool
kubectl label node <node> \
  resource-pool.aibrix.ai/name=latency-pool \
  model.aibrix.ai/warmup-enabled=true
```

Use a namespace where the generated Job template is allowed by Pod Security
admission. The service account creating `ModelWarmup` resources needs the
custom-resource permissions supplied by AIBrix; the controller's service
account needs its documented permissions to create and observe Jobs and read
Nodes. Do not grant arbitrary users permission to create ModelWarmups that can
target pools or mount host paths they do not administer.

The lightweight samples use public `busybox:1.36`,
`aibrix/runtime:v0.8.0`, `nvidia/cuda:12.4.1-base-ubuntu22.04`, and
`sshleifer/tiny-gpt2`. The opt-in engine examples use
`vllm/vllm-openai:v0.10.2`, `lmsysorg/sglang:v0.5.5.post3`, and
`Qwen/Qwen3-0.6B`. Production users should pin image and model digests and use
their registry and artifact credentials as appropriate.

## Apply a sample

```sh
kubectl apply -f samples/modelwarmup/modelwarmup.yaml
kubectl apply -f samples/modelwarmup/model-download.yaml
kubectl apply -f samples/modelwarmup/node-precheck.yaml
kubectl apply -f samples/modelwarmup/gpu-precheck.yaml
kubectl apply -f samples/modelwarmup/vllm-qwen-warmup.yaml
kubectl apply -f samples/modelwarmup/sglang-qwen-warmup.yaml
```

Watch an individual resource with, for example:

```sh
kubectl get modelwarmup vllm-qwen3-0-6b-warmup -w
```

`modelwarmup.yaml` pulls BusyBox and exits successfully. It does not use
`custom` and remains compatible with image-only v1 use.

`model-download.yaml` has no `imagePreload` entries. Its regular custom
container downloads `sshleifer/tiny-gpt2` into the node-local `/models` mount.
In runtime v0.7.0, `--local-dir` is a base directory, so this sample resolves
the artifact to `/models/sshleifer-tiny-gpt2/sshleifer/tiny-gpt2` in the
container and `/var/lib/aibrix/models/sshleifer-tiny-gpt2/sshleifer/tiny-gpt2`
on the node.
`node-precheck.yaml` first checks that the same mount is writable and has at
least 1 GiB available, then uses a finite BusyBox container to mark success.

`gpu-precheck.yaml` runs `nvidia-smi` in an init container that requests one
`nvidia.com/gpu`. Apply it only to GPU nodes with the NVIDIA device plugin
installed and advertising that resource. The following BusyBox regular
container exits successfully.

`vllm-qwen-warmup.yaml` and `sglang-qwen-warmup.yaml` are large, opt-in
combined examples. Each checks writable cache storage, at least 10 GiB free
space, two CPUs, 4 GiB available memory, and Hugging Face DNS. It then preloads
the pinned engine image while downloading
`Qwen/Qwen3-0.6B` into an engine-specific node-local host path. Restrict the
latency-pool selector to only the nodes that should receive these large
artifacts; two nodes are sufficient for a smoke test.

The engine image and downloader are regular containers and may run
concurrently after the precheck init container succeeds. These examples only
prepare the image and model caches; they do not start an inference server.

## Optional P2P distribution with Dragonfly

Start with two or more authorized workers in the same resource pool. Install
Dragonfly separately and check that the nodes can reach its scheduler and
other peers. The platform operator owns the Dragonfly deployment and any
registry mirror configuration. Apply the sample label to workers that meet
the prerequisites for the sample you run.

```sh
kubectl label node <worker1> <worker2> samples.aibrix.ai/dragonfly=true
```

### Prepare model weights

Use [dragonfly-model-download.yaml](dragonfly-model-download.yaml) on workers
with a running `dfdaemon` and its socket at
`/var/run/dragonfly/dfdaemon.sock`. The Job needs permission to use this
node-local socket and to write to `/var/lib/aibrix/models`.

```sh
kubectl apply -f samples/modelwarmup/dragonfly-model-download.yaml
kubectl get modelwarmup dragonfly-model-download -w
```

The sample uses Dragonfly Client v1.5.14. It downloads and checks a fixed
revision of a public tiny Llama test model, then publishes the complete
directory. The explicit `--scheduling-policy always` sends even this small
model's files through peer scheduling. Under the default `auto` policy,
small files may go directly to the source. A fresh run with an unavailable
node peer fails the download Job. Source fallback within Dragonfly follows
the platform's Dragonfly configuration.

If checksum validation fails for a published model directory, move it aside
on the affected node once no workload is using it. Then apply a copy of the
sample with a new `metadata.name` to start a new `Once` run. A forced Job
termination can leave `.dragonfly-model.*` staging directories under
`/var/lib/aibrix/models`. Clean up those directories after confirming that no
active Job is using them.

To load the prepared weights, schedule the serving workload on a worker
where the Job completed. Mount the same node directory read-only and pass the
explicit model path to the engine. For example, merge these fields into an
existing vLLM Pod template and keep its normal GPU configuration.

```yaml
containers:
- name: vllm
  command: ["vllm", "serve"]
  args:
  - /models/tiny-random-llama-9fb191250dd56d0ba7ec9785a025ed29c03d5998
  volumeMounts:
  - name: model-cache
    mountPath: /models
    readOnly: true
volumes:
- name: model-cache
  hostPath:
    path: /var/lib/aibrix/models
    type: Directory
```

### Preload a container image

Configure the [containerd registry mirror](https://d7y.io/docs/operations/integrations/container-runtime/containerd/)
on the workers that will pull the image. Copy the existing image-preload
sample.

```sh
cp samples/modelwarmup/modelwarmup.yaml /tmp/dragonfly-image-warmup.yaml
```

Edit the copy.

1. Set `metadata.name` to `dragonfly-image-warmup`.
2. Set `spec.imagePreload.images[0].image` to a BusyBox image pinned by
   digest, such as `docker.io/library/busybox@sha256:<digest>`. Use the
   registry configured for the mirror and keep the sample's
   `sh -c "exit 0"` command.
3. Add `samples.aibrix.ai/dragonfly: "true"` to
   `spec.targets[0].nodeSelector.matchLabels` and keep the resource-pool label.

```sh
kubectl apply -f /tmp/dragonfly-image-warmup.yaml
kubectl get modelwarmup dragonfly-image-warmup -w
```

Kubelet asks the container runtime to pull the image. The runtime mirror
determines the transport and any registry fallback.

### Inspect failed warmups

Watch the ModelWarmup phase until it reaches `Succeeded`. If it reaches
`Failed` or `Degraded`, inspect the resource status and the Jobs owned by it.

```sh
warmup=dragonfly-model-download   # or dragonfly-image-warmup
kubectl describe modelwarmup "$warmup"
uid=$(kubectl get modelwarmup "$warmup" -o jsonpath='{.metadata.uid}')
kubectl get jobs -l "model.aibrix.ai/warmup=$uid"
kubectl logs job/JOB_NAME
```

Use the failed Job's name in the last command. Its events are available with
`kubectl describe job JOB_NAME` if the Pod failed to start.

### Verify peer traffic

Start with empty caches for the artifacts under test, including the relevant
Dragonfly task cache. Compare direct and P2P runs using identical artifacts
and similarly sized worker groups. Record bytes at the origin and the time
from applying the resource to its `Complete` condition. Check that the
downloaded model loads or that the preloaded image runs.

Inspect Dragonfly's
[client transfer counters](https://d7y.io/docs/operations/best-practices/observability/prometheus-metrics/)
before and after each trial. An increase in `dragonfly_client_download_traffic`
with `type="REMOTE_PEER"` confirms that peers served bytes. Compare it with
`BACK_TO_SOURCE` and the origin logs. `ModelWarmup.status` reports which Jobs
completed. A completed Job can have fetched every byte from the origin.

## Cache and completion behavior

The download and precheck samples use a `hostPath` volume under
`/var/lib/aibrix/models`, created with `DirectoryOrCreate` and mounted as
`/models`. The engine examples use separate `vllm-qwen3-0.6b` and
`sglang-qwen3-0.6b` subdirectories. These are node-local caches, not a
distributed store or a guarantee that an inference Pod uses the artifact.
Verify ownership, permissions, capacity, backup, and cleanup rules on every
target node before adopting this pattern.

All image-preload and custom container commands must finish and exit zero. A
server, shell, downloader that waits indefinitely, or other long-running
process keeps the node Job running until its configured timeout. Init containers
run one at a time; after they succeed, generated image-preload containers and
custom regular containers start as the Job's regular containers.

ModelWarmup reports the aggregate Job result in its status. A successful result
only records that the finite Job succeeded at that time. Kubelet image garbage
collection can remove pulled images, and node replacement, cleanup, or storage
failure can remove host-path artifacts. Treat both as caches and provide a
durable artifact source for production workloads.

ModelWarmup admission does not perform full native Job/Pod validation. Job
creation rejected by the API server is logged and requeued. Because no Job
exists, bounded Job diagnostics and ModelWarmup failure status may not show the
rejection; check controller logs when progress stalls.

## Security notes

`hostPath` exposes node filesystem data to the generated Job and is unsuitable
for untrusted ModelWarmup authors. Directory creation can also require node
filesystem permissions that restricted admission policies reject. These samples
do not set `privileged: true`; do not add privilege, host networking, host PID,
or broader host mounts merely to make a sample work. Use least-privilege RBAC,
dedicated target pools, and a namespace admission policy appropriate for the
container images and host-path access you authorize.

Mounting the Dragonfly socket read-only still allows the Job to call the
node's `dfdaemon` API. Grant access to the socket and the writable model
cache only to trusted ModelWarmup authors.

Automatic service-account token mounting is disabled, but explicit projected
`serviceAccountToken` volumes are allowed. Tokens use the Pod's service account
permissions (normally the namespace default service account); secret volumes can
also expose namespace credentials. Keep service-account permissions minimal and
restrict the token and secret volumes that ModelWarmup authors may request.
