# llm-d Deployment Notes

This directory contains Kubernetes manifests and Helm values for the Brixbench
llm-d scenario. It does not contain container images or model artifacts.

Model files are expected to already exist on the test cluster under
`/data01/models`, and the modelserver manifest mounts that host path.

## Images

The llm-d Brixbench testdata uses internal registry image names. If an image is
missing, mirror the matching upstream image before running the smoke benchmark.

Modelserver image:

- Source: `guides/recipes/modelserver/components/images/gpu-vllm`
- Upstream image: `vllm/vllm-openai:v0.23.0`
- Brixbench image: `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/llmd-vllm-openai:v0.23.0`

Routing sidecar image:

- Source: `guides/recipes/modelserver/components/images/routing-sidecar`
- Upstream image: `ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0`
- Brixbench image: `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/llmd-router-disagg-sidecar:v0.9.0`

Router/EPP image:

- Source: `oci://ghcr.io/llm-d/charts/llm-d-router-standalone`
- Chart version: `v0.9.0`
- Upstream EPP image: `ghcr.io/llm-d/llm-d-router-endpoint-picker:v0.9.0`
- Brixbench EPP image: `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/llm-d-router-endpoint-picker:v0.9.0`
- Upstream Envoy image: `docker.io/envoyproxy/envoy:distroless-v1.33.2`
- Brixbench Envoy image: `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/envoyproxy-envoy:distroless-v1.33.2`
