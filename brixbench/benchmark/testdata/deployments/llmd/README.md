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

## Routing Policy

The checked-in llm-d scenario uses the upstream llm-d v0.8.1 P/D
disaggregation **cache-plus-load** policy:

```text
brixbench/benchmark/testdata/deployments/llmd/router/policies/pd-disaggregation-official.yaml
```

It keeps prefill and decode endpoints separate and uses the Endpoint Picker
(EPP) default `max-score-picker` with cache and load signals:

- prefill: prefix-cache (weight 3), queue (2), KV-cache utilization (2)
- decode: active requests (2), prefix-cache (3)

The policy is selected by the scenario's ordered `controlplane` Helm values
files:

```yaml
controlplane:
- brixbench/benchmark/testdata/deployments/llmd/router/base-values.yaml
- brixbench/benchmark/testdata/deployments/llmd/router/policies/pd-disaggregation-official.yaml
```

To benchmark another llm-d policy, create a new values overlay under
`brixbench/benchmark/testdata/deployments/llmd/router/policies/` and reference
it as the second `controlplane` file in a new scenario test under
`brixbench/benchmark/testdata/scenarios/`. The runner passes the files to Helm
in order, so the policy overlay replaces the EPP plugin configuration without
changes to the deployer, modelserver manifest, or benchmark client.

llm-d endpoint routing is a filter, scorer, and picker pipeline rather than a
single `router-mode` value. In particular, `round-robin-fairness-policy`
controls flow-control queue fairness and is not equivalent to Dynamo's
`--router-mode round-robin`. See the [llm-d EPP scheduling
reference](https://github.com/llm-d/llm-d/blob/main/docs/architecture/core/router/epp/scheduling.md)
for supported filters, scorers, and pickers.
