# Preparing model caches for ModelClaim

Keep the ModelClaim artifact URL and engine settings unchanged. Prepare files
ahead of demand with a ModelWarmup custom Job or an initContainer. The runtime
reports newly discovered local caches through its existing snapshot field, and
the controller's existing cache preference applies only among feasible Pods.

## Build and configure

Build a preparation image from this feature's source:

```sh
docker build --target runtime -f build/container/Dockerfile.python \
  -t aibrix/runtime:cache-prepare .
```

Make that image available to the target cluster. The warm-pool runtime also
needs this source version for cache discovery; its engine dependencies remain
those of the existing kvcached runtime image.

Authorize namespace and worker-node membership as in [README.md](README.md),
then apply [model-cache.yaml](model-cache.yaml). Its host paths match
[warm-runtime-pool.yaml](../modelclaim/warm-runtime-pool.yaml). Use one test
node first, check Warmup completion, then apply your original Claim YAML.

## Sources and native layouts

The same command supports these forms:

```sh
aibrix_prepare_model_cache --model-uri huggingface://Qwen/Qwen3-0.6B
aibrix_prepare_model_cache --model-uri s3://my-bucket/releases/model-a
aibrix_prepare_model_cache --model-uri tos://my-bucket/releases/model-b
aibrix_prepare_model_cache --model-uri gcs://my-bucket/releases/model-c
```

HF also accepts `hf://` and bare `org/repo` IDs. GCS also accepts `gs://`.
Object-store sources must be model directory prefixes, not single weight files
or signed URLs. Use ordinary environment/secret credentials rather than secrets
embedded in model URLs. Set `--revision` only when you intentionally need an HF
reference already used by your existing engine configuration; it is not required.

| Source | Default destination | Required matching configuration |
|---|---|---|
| HF | Hub client's `HF_HUB_CACHE`, including native refs/blobs/snapshots | Same effective HF cache, endpoint and intended reference |
| S3 | `DOWNLOADER_LOCAL_DIR/<prefix-basename>` | Existing AWS credentials, region and endpoint configuration |
| TOS | `DOWNLOADER_LOCAL_DIR/<prefix-basename>` | Existing TOS credentials, endpoint, region and downloader version |
| GCS/GS | `DOWNLOADER_LOCAL_DIR/<prefix-basename>` | Existing Application Default Credentials / workload identity |

`--cache-dir` overrides that backend's root for preparation only. The engine
must use the same underlying root; it does not receive this CLI argument.
Do not set per-Claim download directories that point elsewhere and expect these
cache hints to describe that other directory.

HF preparation uses `snapshot_download(cache_dir=...)`, not a plain local_dir
copy. Object-store preparation calls the existing downloader and atomically
writes a small receipt under `<DOWNLOADER_LOCAL_DIR>/.aibrix/prepared/`.
Receipt/index JSON is bounded to 2 MiB; an oversized preparation receipt fails
before publication instead of silently producing an undiscoverable cache. Empty
auxiliary files are allowed, while required model files must be nonempty.
Receipts record URL, source-configuration fingerprint, relative file paths and
file metadata; they do not contain credentials, replace model files, or override
the normal loader's remote ETag/size validation. Explicit force-download options
retain their existing behavior.

Object downloaders currently choose directories by prefix basename. Different
sources with the same basename need different prefixes or separate cache roots
and pools. The preparer rejects a conflicting valid preparation rather than
overwriting it. Changing endpoints/regions or overwriting cached files withdraws
the affected preparation hint. Normal loader behavior is otherwise unchanged.

## Discovery and guarantees

The runtime refreshes a local index approximately every 10 seconds. Snapshot
reads do not wait for scanning and never perform remote downloads. A failed
scan withdraws the new hints for the affected backend; a stuck scan cannot keep
old new-style hints visible for more than two refresh intervals. Legacy served
markers remain compatible and are not retroactively guaranteed to be verified.

New hints require a recognizable Transformers model layout: model configuration,
tokenizer configuration and tokenizer files, plus complete single-file or indexed
safetensors/PyTorch weights. The checks do not restrict model family, but do not
prove GPU/engine compatibility or cryptographic integrity. Unsupported layouts
continue to use normal on-demand loading without a new hint. Missing files,
broken or escaping links, and incomplete preparations do not produce new hints.

HF default-URL hints describe the locally cached `main` reference. They do not
freeze upstream `main` or promise a particular engineConfig revision is cached.
If upstream content changes, the unchanged engine/downloader may download it.
Object-store cache hits still require the normal source credentials and metadata
checks. Neither backend promises zero metadata requests or offline operation.

Mounts and UID/GID permissions must allow both preparation and normal engine
cache maintenance. A node's files are not automatically available to every Pod
on that node, or to other nodes in the resource pool. Restrict shared roots to
trusted, authorized tenants; pool labels do not grant filesystem authorization.

Warmup success is preparation success, not model readiness. Cache preference
never overrides GPU capacity, priority or pool constraints. Cache loss follows
the existing loader's behavior; arbitrary corruption is not guaranteed to be
automatically repaired. Deleting a Claim or Warmup does not delete shared files.
Automatic eviction, multi-version-exact scheduling and distributed filesystems
are outside this feature.

## Local verification

From `python/aibrix`, use the repository's Poetry environment:

```sh
poetry run pytest tests/downloader/test_hf_cache.py \
  tests/downloader/test_model_cache.py \
  tests/downloader/test_native_model_cache.py tests/runtime/test_model_cache.py
```

The native HF tests run a loopback HTTP Hub fixture and the real Hub client,
including repeated consumer reads, concurrent preparation and interrupted
downloads. Object-provider tests run the actual S3/TOS/GCS downloader code with
transport-only SDK fakes, checking payload reuse, freshness and cache loss.
These tests do not claim access to live cloud accounts or GPU inference.
