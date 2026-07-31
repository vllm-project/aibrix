# Build SGLang Image with AIBrix KVCache

This directory contains the Dockerfile to build a SGLang image with AIBrix KVCache integrated.

## Build Arguments

The following arguments can be provided during the build process:

* `AIBRIX_REPO`: The URL of the AIBrix git repository.
  * Default: `https://github.com/vllm-project/aibrix`
* `AIBRIX_BRANCH`: The branch of the AIBrix repository to use.
  * Default: `main`
* `TORCH_VERSION`: The version of PyTorch to install. This should be compatible with the sglang base image.
  * Default: `2.8.0`
* `SGLANG_VERSION`: The version of the `lmsysorg/sglang` image to use as a base.
  * Default: `latest`

## How to Build

You can build the image using the `docker build` command. For example:

```bash
DOCKER_BUILDKIT=1 docker build . --target sglang --tag aibrix/sglang-aibrix-kvcache:latest-20251028
```

You can also override the build arguments:

```bash
DOCKER_BUILDKIT=1 docker build . --target sglang --tag aibrix/sglang-aibrix-kvcache:v0.5.4-20251028 --build-arg SGLANG_VERSION=v0.5.4
```
