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

The public `busybox:1.36`, `aibrix/runtime:v0.7.0`,
`nvidia/cuda:12.4.1-base-ubuntu22.04`, and `sshleifer/tiny-gpt2` choices keep
the samples small and reproducible enough for demonstration. Production users
should validate their exact image commands, pin image/model revisions, and use
their registry and artifact credentials as appropriate.

## Apply a sample

```sh
kubectl apply -f samples/modelwarmup/modelwarmup.yaml
kubectl apply -f samples/modelwarmup/model-download.yaml
kubectl apply -f samples/modelwarmup/node-precheck.yaml
kubectl apply -f samples/modelwarmup/gpu-precheck.yaml
kubectl apply -f samples/modelwarmup/combined-warmup.yaml
```

Watch an individual resource with, for example:

```sh
kubectl get modelwarmup combined-runtime-and-model-warmup -w
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

`combined-warmup.yaml` runs a generic cache precheck first, then starts the
generated BusyBox image-preload container and the custom downloader as regular
containers. The regular containers may run concurrently; only init containers
are sequential.

## Cache and completion behavior

The download, node-precheck, and combined samples use a `hostPath` volume at
`/var/lib/aibrix/models`, created with `DirectoryOrCreate` and mounted as
`/models`. This is a node-local cache shared by Pods that mount the same host
path; it is neither a distributed store nor a guarantee that an inference Pod
uses the artifact. Verify ownership, permissions, capacity, backup, and cleanup
rules on every target node before adopting this pattern.

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

Automatic service-account token mounting is disabled, but explicit projected
`serviceAccountToken` volumes are allowed. Tokens use the Pod's service account
permissions (normally the namespace default service account); secret volumes can
also expose namespace credentials. Keep service-account permissions minimal and
restrict the token and secret volumes that ModelWarmup authors may request.
