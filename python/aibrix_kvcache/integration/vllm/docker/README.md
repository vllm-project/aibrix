# Build vLLM Image with AIBrix KVCache

This directory contains the Dockerfile to build a vLLM image with AIBrix KVCache integrated.

## Build Arguments

The following arguments can be provided during the build process:

* `AIBRIX_REPO`: The URL of the AIBrix git repository.
  * Default: `https://github.com/vllm-project/aibrix`
* `AIBRIX_BRANCH`: The branch of the AIBrix repository to use.
  * Default: `main`
* `TORCH_VERSION`: The version of PyTorch to install. This should be compatible with the vLLM base image.
  * Default: `2.8.0`
* `VLLM_VERSION`: The version of the `vllm/vllm-openai` image to use as a base.
  * Default: `v0.10.2`

## How to Build

You can build the image using the `docker build` command. For example:

```bash
DOCKER_BUILDKIT=1 docker build . --target vllm-openai --tag aibrix/vllm-openai-aibrix-kvcache:v0.10.2-20251022
```

You can also override the build arguments:

```bash
DOCKER_BUILDKIT=1 docker build . --target vllm-openai --tag aibrix/vllm-openai-aibrix-kvcache:v0.9.1-20251022 --build-arg VLLM_VERSION=v0.9.1
```
