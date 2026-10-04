# Jev-compatible external replica router

This example implements the AIBrix external replica routing protocol in Go and
delegates candidate selection to a TypeSafe/Jev-compatible
`POST /v1/systemone` service. It is a semantic routing reference: the remote
decision model can consider topology, replica role, trusted policy attributes,
and current load together instead of applying a fixed sorting rule.

The service cannot add a backend, change the requested model, or return an
arbitrary address. A Jev choice is accepted only when it exactly matches a
candidate ID from the current Gateway request.

## Current scope: single-target routing

This reference implements the current
`routing.aibrix.ai/v1alpha1` single-target contract. Jev chooses exactly one
candidate Pod and the adapter returns one `status.target` to Gateway.

Candidate attributes can include `prefill` and `decode` roles so a policy can
reason about them, but the role is only a feature of this one-target choice.
Selecting a role-labelled candidate Pod does not select a P/D pair, run a
prefill leg, transfer KV state, or prove a decode handoff. Use AIBrix's built-in
`pd` strategy for the current disaggregated workflow; external control of that
paired workflow is future scope.

## Request flow

```text
inference request
       |
       v
AIBrix Gateway
       | ReplicaSelectionRequest
       v
jev-decision-service
       | TypeSafe /v1/systemone choice
       v
Jev-compatible decision model
       | choice + confidence + probabilities
       v
jev-decision-service
       | validated ReplicaSelectionResponse
       v
AIBrix Gateway -> selected Pod
```

For every Gateway request, the adapter places the model, policy mode, trusted
policy context, and complete candidate snapshot in `state`. Candidate IDs
become the `choice` criteria. Their descriptions summarize ports, attributes,
and available metrics.

## Configuration

| Variable | Required | Default | Description |
|---|:---:|---|---|
| `JEV_ENDPOINT` | yes | | Complete `/v1/systemone` URL. |
| `JEV_TIMEOUT` | no | `10s` | Timeout for one decision request. |
| `JEV_API_KEY` | no | | Optional bearer token sent to the decision service. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |
| `ROUTING_INSTRUCTIONS` | no | built in | Natural-language candidate selection rule. |

The built-in rule asks the model to jointly consider topology,
prefill/decode role, running requests, engine utilization, KV-cache pressure,
and trusted policy context. Set `ROUTING_INSTRUCTIONS` to demonstrate a
different semantic policy.

## Run locally

Start a Jev-compatible service first. For example, if it listens on port 8090:

```bash
export JEV_ENDPOINT=http://127.0.0.1:8090/v1/systemone
go run ./samples/external-replica-router/jev-decision-service
```

Check process health:

```bash
curl -s http://127.0.0.1:8080/healthz
```

`/healthz` is a liveness endpoint. It does not claim that the configured Jev
service is reachable.

## Try a selection

```bash
curl -s http://127.0.0.1:8080/v1alpha1/select \
  -H 'Content-Type: application/vnd.aibrix.external-routing+json;version=v1alpha1' \
  -d '{
    "apiVersion": "routing.aibrix.ai/v1alpha1",
    "kind": "ReplicaSelectionRequest",
    "metadata": {"requestId": "req-jev-1"},
    "spec": {
      "model": "qwen2-7b",
      "policyMode": "Advisory",
      "policyContext": {
        "attributes": {"requestClass": "interactive"}
      },
      "candidates": [
        {
          "id": "default/qwen-prefill-a",
          "ports": [8000],
          "attributes": {
            "role": "prefill",
            "topology.kubernetes.io/zone": "zone-a"
          },
          "metrics": {
            "runningRequests": 1,
            "engineUtilization": 0.35,
            "kvCacheUsage": 0.20
          }
        },
        {
          "id": "default/qwen-decode-b",
          "ports": [8000],
          "attributes": {
            "role": "decode",
            "topology.kubernetes.io/zone": "zone-b"
          },
          "metrics": {
            "runningRequests": 4,
            "engineUtilization": 0.72,
            "kvCacheUsage": 0.64
          }
        }
      ]
    }
  }'
```

A successful response uses the AIBrix versioned media type and selects one ID
from the request:

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionResponse",
  "metadata": {"requestId": "req-jev-1"},
  "status": {
    "decision": "Selected",
    "target": {"id": "default/qwen-prefill-a", "port": 8000}
  }
}
```

## Generated Jev request

The request above is converted to this shape (descriptions abbreviated):

```json
{
  "state": {
    "model": "qwen2-7b",
    "policyMode": "Advisory",
    "policyContext": {
      "attributes": {"requestClass": "interactive"}
    },
    "candidates": [
      {
        "id": "default/qwen-decode-b",
        "ports": [8000],
        "attributes": {"role": "decode"},
        "metrics": {"runningRequests": 4}
      },
      {
        "id": "default/qwen-prefill-a",
        "ports": [8000],
        "attributes": {"role": "prefill"},
        "metrics": {"runningRequests": 1}
      }
    ]
  },
  "questions": {
    "replica": {
      "type": "choice",
      "instructions": "Select the best replica by jointly considering node topology, prefill/decode role, current request load, engine utilization, KV cache pressure, and trusted policy context. Prefer a healthy, suitable, less-loaded candidate.",
      "criteria": {
        "default/qwen-decode-b": "ports=[8000], role=decode, runningRequests=4, ...",
        "default/qwen-prefill-a": "ports=[8000], role=prefill, runningRequests=1, ..."
      }
    }
  }
}
```

The adapter reads `answers.replica.choice`, `confidence`, and
`probabilities`. Confidence is diagnostic only; this reference example does
not impose a threshold.

## Configure AIBrix Gateway

Point the external router at the complete operation URL and allowlist only the
attributes and metrics the decision model should see:

```yaml
- name: AIBRIX_EXTERNAL_ROUTER_ENDPOINT
  value: http://jev-decision-service.aibrix-system.svc:8080/v1alpha1/select
- name: AIBRIX_EXTERNAL_ROUTER_POLICY_MODE
  value: Advisory
- name: AIBRIX_EXTERNAL_ROUTER_FAILURE_MODE
  value: FailOpen
- name: AIBRIX_EXTERNAL_ROUTER_FALLBACK
  value: least-request
- name: AIBRIX_EXTERNAL_ROUTER_TIMEOUT
  value: 15s
- name: AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES
  value: role,topology.kubernetes.io/zone,storm-service-name
- name: AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS
  value: runningRequests,engineUtilization,kvCacheUsage
```

The 15-second example timeout accommodates a CPU-hosted decision model. Use a
tighter value appropriate for the deployed model and latency objective.

Clients select the strategy with the existing inference request header:

```text
routing-strategy: external
```

### Candidate port labels

Every candidate Pod must expose a routable model port to AIBrix. Set the
standard AIBrix model and port labels:

```yaml
metadata:
  labels:
    model.aibrix.ai/name: qwen2-7b
    model.aibrix.ai/port: "8000"
```

This requirement is stricter than some built-in routers. A built-in path may
fall back to port 8000 when the label is absent, while the external router must
serialize an explicit allowed-port snapshot and validate the returned target
against it. Without the label, request construction fails with
`candidate <namespace>/<pod> has no routable port`; in `FailOpen` mode the
request then uses the configured local fallback without calling the decision
service. The Prometheus signature is an `invalid_response` increment with no
corresponding external-router duration observation.

## Build a container

Run the build from the AIBrix repository root:

```bash
docker build \
  -f samples/external-replica-router/jev-decision-service/Dockerfile \
  -t aibrix-jev-decision-service:dev .
```

Run it against a Jev endpoint reachable from the container:

```bash
docker run --rm -p 8080:8080 \
  -e JEV_ENDPOINT=http://host.docker.internal:8090/v1/systemone \
  aibrix-jev-decision-service:dev
```

On Linux, configure the appropriate host-gateway mapping or use a routable
service address instead of `host.docker.internal`.

## Failure behavior

| Condition | HTTP result | Gateway behavior |
|---|---:|---|
| Invalid AIBrix request | 400 | Protocol/system failure handling |
| Jev timeout, connection failure, or non-2xx | 503 | Configured external-router failure mode |
| Malformed Jev response or choice outside snapshot | 502 | Configured external-router failure mode |
| Valid choice | 200 `Selected` | Gateway validates and routes |

With the recommended `Advisory` + `FailOpen` configuration, an unavailable
decision model causes Gateway to run `least-request` over the same locally
validated candidate list.

## Security and limitations

- The external router receives only the candidate snapshot and explicitly
  allowlisted policy data. It does not receive prompts, inference bodies,
  arbitrary client headers, Pod IPs, or client authorization.
- The selected ID and port are validated against the exact request snapshot;
  the model cannot invent a backend.
- Natural-language routing is not deterministic. Probabilities describe the
  model's preference among the supplied candidates, not objective correctness.
- This is a reference implementation. Production deployments should add TLS,
  workload authentication, rate limits, availability controls, metrics, and a
  reviewed policy prompt.

## Verify

```bash
go test ./samples/external-replica-router/jev-decision-service
go test -race ./samples/external-replica-router/jev-decision-service
go vet ./samples/external-replica-router/jev-decision-service
```
