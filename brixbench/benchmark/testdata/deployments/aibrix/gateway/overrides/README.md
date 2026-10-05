# Gateway Override Drafts

This directory is reserved for benchmark-facing gateway override drafts.

## Layout

- `vllm-pd/resources.yaml`: Additional Kubernetes resources applied after gateway env patching.
- `vllm-pd/notes.md`: Assumptions and rollout behavior for the draft.

## Intent

These files are designed for future test-case interfaces where a benchmark case
can provide:

- gateway runtime env overrides at the scenario level
- extra Kubernetes resource files for Gateway API resources

## Rollout Behavior

Runtime env changes to the gateway plugin pod template require a Deployment
rollout after the deployer patches the shared Deployment. Examples include:

- `ROUTING_ALGORITHM`

Resources such as `HTTPRoute`, `ReferenceGrant`, and standalone `Service`
objects do not require a gateway pod rollout by themselves. They are expected to
be applied after the gateway runtime configuration has been updated.
