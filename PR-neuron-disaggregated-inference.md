## Pull Request Description

This PR adds support for AWS Trainium2 (Neuron) instances in the aibrix disaggregated inference (P/D) routing implementation. The changes enable aibrix to route requests through the prefill→decode flow using NIXL for KV cache transfer over EFA (Elastic Fabric Adapter).

### Changes

1. **Documentation**: Added comprehensive setup guide for Neuron disaggregated inference (`docs/neuron-disaggregated-inference-setup.md`) covering:
   - EKS nodegroup configuration with EFA networking
   - Separate prefill/decode Deployment architecture for independent scaling
   - Aibrix cluster setup and configuration
   - Gateway plugin code changes for pd routing
   - Testing procedures

2. **P/D Routing Logic** (in `pkg/plugins/gateway/algorithms/pd_disaggregation.go`):
   - Pod filtering by `role-name` label (prefill/decode)
   - Prefill request modification (`max_tokens=1`)
   - KV transfer params extraction and forwarding to decode

3. **HTTPRoute Validation** (in `pkg/plugins/gateway/gateway.go`):
   - Skip HTTPRoute status validation for pd routing algorithm

### Request Flow

```
Client → Gateway → Prefill Pod (max_tokens=1) → NIXL KV Transfer → Decode Pod → Response
```

### Pod Configuration

Users must label pods with:
- `role-name: prefill` or `role-name: decode`
- `model.aibrix.ai/name: <model-name>`

And annotate with:
- `model.aibrix.ai/port: "8000"`

### Testing

Tested with:
- AWS Trainium2 instances (trn2.48xlarge)
- vLLM with Neuron support and NIXL connector
- Llama 3.1 8B model
- EFA-enabled EKS cluster

## Related Issues

Resolves: #N/A (New feature - Neuron/Trainium2 support for disaggregated inference)

---

### Submission Checklist

- [x] PR title includes appropriate prefix(es): `[Docs][API]`
- [x] Changes are clearly explained in the PR description
- [x] New and existing tests pass successfully
- [x] Code adheres to project style and best practices
- [x] Documentation updated to reflect changes
- [x] Thorough testing completed, no regressions introduced
