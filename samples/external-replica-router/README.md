# External replica router sample

This sample shows how an operator-owned HTTP service can select one replica
after AIBrix Gateway has resolved the model and filtered the available pods. It
is replica routing, not semantic or model routing: the service cannot change the
model, add a backend, or return an arbitrary address.

The sample policy runs in `Advisory` mode. It:

- receives only the candidate snapshot and explicitly allowlisted data;
- requires an H100 candidate for `premium-model`;
- prefers `PREFERRED_ZONE` when at least one eligible candidate is in that zone;
- sorts by candidate ID for deterministic tie-breaking;
- returns `NoDecision` when no premium candidate is eligible, causing Gateway
  to run the configured `least-request` fallback.

If the same service receives an `Authoritative` request and no candidate is
eligible, it returns `Denied` instead; this keeps every response legal for the
request's policy mode.

The sample is intentionally small and stateless. Production services should add
authentication, TLS, availability controls, bounded logging, and their own
policy data source.

## Request flow and ownership

1. The inference client sends a normal request to AIBrix Gateway. To select this
   strategy explicitly, it includes `routing-strategy: external`.
2. Gateway resolves the requested model and applies its local candidate safety
   filters.
3. Gateway creates a `ReplicaSelectionRequest` containing a snapshot of the
   remaining candidates and sends it to the configured operation URL.
4. The decision service returns `Selected`, `NoDecision`, or `Denied`.
5. Gateway validates the response against the exact request snapshot. Gateway,
   not the decision service, resolves the pod IP, performs final admission and
   accounting, and mutates the Envoy request.

The external service must never treat the request as a reservation. Candidate
metrics are observational snapshots and can change before the inference request
reaches the selected pod.

## HTTP operation

Gateway sends:

```text
POST <AIBRIX_EXTERNAL_ROUTER_ENDPOINT>
Content-Type: application/vnd.aibrix.external-routing+json;version=v1alpha1
Accept: application/vnd.aibrix.external-routing+json;version=v1alpha1
X-Request-Id: <metadata.requestId>
Authorization: Bearer <token>     # only when a token file is configured
traceparent: <W3C trace context>  # when a valid current trace is available
```

The endpoint is the complete operation URL. Gateway does not append a path,
follow redirects, retry, or forward the inference request's authorization and
arbitrary headers. A protocol response is valid only when the HTTP status is
`200` and `Content-Type` is the versioned media type shown above.

## Complete request example

This example includes every optional request section. Fields that are not
configured or unavailable are omitted rather than encoded as `null`.

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionRequest",
  "metadata": {
    "requestId": "req-123"
  },
  "spec": {
    "model": "premium-model",
    "policyMode": "Advisory",
    "policyContext": {
      "attributes": {
        "tenantTier": "gold",
        "requestClass": "interactive"
      }
    },
    "candidates": [
      {
        "id": "default/premium-model-h100-6f9478d69f-2k9mt",
        "ports": [8000],
        "attributes": {
          "routing.example.com/accelerator-class": "h100",
          "topology.kubernetes.io/zone": "cn-east-1a"
        },
        "metrics": {
          "runningRequests": 4,
          "engineUtilization": 0.61,
          "kvCacheUsage": 0.48
        }
      },
      {
        "id": "default/premium-model-h100-6f9478d69f-r8wq7",
        "ports": [8000],
        "attributes": {
          "routing.example.com/accelerator-class": "h100",
          "topology.kubernetes.io/zone": "cn-east-1b"
        },
        "metrics": {
          "runningRequests": 1
        }
      }
    ]
  }
}
```

### Request fields

| Field | Required | Meaning and constraints |
|---|---|---|
| `apiVersion` | yes | Must be `routing.aibrix.ai/v1alpha1`. |
| `kind` | yes | Must be `ReplicaSelectionRequest`. |
| `metadata.requestId` | yes | Non-empty request correlation ID, at most 256 UTF-8 bytes. It also appears in `X-Request-Id`. |
| `spec.model` | yes | Model already selected by Gateway. The service cannot replace it. |
| `spec.policyMode` | yes | `Advisory` or `Authoritative`, matching Gateway process configuration. |
| `spec.policyContext` | no | Container for trusted, operator-allowlisted policy data. Omitted when empty. |
| `spec.policyContext.attributes` | no | At most 32 trusted key/value pairs. These do not come directly from arbitrary client headers. |
| `spec.candidates` | yes | Non-empty, lexicographically sorted list of request-local candidates. Order carries no preference. |
| `spec.candidates[].id` | yes | Stable request-local identity in `namespace/pod-name` form. Return this exact value in `status.target.id`. |
| `spec.candidates[].ports` | yes | Unique sorted ports in the range 1-65535. The response may select only one of these ports. |
| `spec.candidates[].attributes` | no | Pod labels selected by `AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES`. Omitted when none are valid. |
| `spec.candidates[].metrics` | no | Fixed typed metrics selected by `AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS`. Missing or invalid values are omitted individually. |
| `metrics.runningRequests` | no | Non-negative pod-wide live request count from one Gateway cache snapshot; it is not a per-port reservation. |
| `metrics.engineUtilization` | no | Finite ratio in `[0,1]` from the requested model's cached engine metric. Currently populated for engines that expose the mapped metric, including xLLM. |
| `metrics.kvCacheUsage` | no | Finite ratio in `[0,1]`; `1.0` means the model's KV cache is full. |

Attribute keys must match `[A-Za-z][A-Za-z0-9_./-]{0,127}` and values must be
at most 256 UTF-8 bytes. Candidate and policy attribute maps contain at most 32
entries each.

Gateway never includes pod IPs, prompts, tokenized text, inference bodies,
client authorization, or arbitrary client headers in this request.

## Response examples

Every valid response uses the same API version and response kind, and echoes
`metadata.requestId` exactly. `metadata.decisionId` is an optional bounded ID
owned by the decision service.

### Selected

`Selected` is valid in both policy modes.

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionResponse",
  "metadata": {
    "requestId": "req-123",
    "decisionId": "decision-456"
  },
  "status": {
    "decision": "Selected",
    "target": {
      "id": "default/premium-model-h100-6f9478d69f-2k9mt",
      "port": 8000
    }
  }
}
```

`target.id` must exactly match a candidate ID from this request. `target.port`
must be one of that candidate's advertised ports. The port may be omitted only
when the selected candidate has exactly one port; Gateway then fills it locally.
The service cannot return an IP address or URL.

### NoDecision

`NoDecision` is valid only in `Advisory` mode. Gateway runs the explicitly
configured local fallback. It is a successful policy response and does not count
as a circuit-breaker failure.

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionResponse",
  "metadata": {
    "requestId": "req-123"
  },
  "status": {
    "decision": "NoDecision",
    "reason": "NoApplicablePolicy"
  }
}
```

### Denied

`Denied` is valid only in `Authoritative` mode. Gateway returns HTTP 403 with
the fixed client error code `external_policy_denied`; it does not call a
fallback or inference backend. Denial is a valid policy result and does not
count as a circuit-breaker failure.

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionResponse",
  "metadata": {
    "requestId": "req-123",
    "decisionId": "decision-789"
  },
  "status": {
    "decision": "Denied",
    "reason": "CompliancePolicy"
  }
}
```

### Response fields

| Field | Required | Meaning and constraints |
|---|---|---|
| `apiVersion` | yes | Must be `routing.aibrix.ai/v1alpha1`. |
| `kind` | yes | Must be `ReplicaSelectionResponse`. |
| `metadata.requestId` | yes | Must exactly equal the request's `metadata.requestId`. |
| `metadata.decisionId` | no | Decision-service correlation ID, at most 256 UTF-8 bytes. |
| `status.decision` | yes | `Selected`, `NoDecision`, or `Denied`. Mode legality is enforced by Gateway. |
| `status.target` | conditional | Required only for `Selected`; forbidden for `NoDecision` and `Denied`. |
| `status.target.id` | conditional | Exact ID of a candidate in the request snapshot. |
| `status.target.port` | conditional | Candidate port. May be omitted only when that candidate advertised one port. |
| `status.reason` | no | Diagnostic reason, at most 256 UTF-8 bytes. It is not copied into the client error body. |

Unknown response fields are ignored for additive alpha evolution. Malformed
JSON, duplicate object members, a second trailing JSON value, missing or null
required fields, wrong media type, mismatched request IDs, invalid decisions,
and targets outside the snapshot are protocol failures.

## Policy and failure behavior

| Mode | Decision/result | Gateway behavior |
|---|---|---|
| Advisory | `Selected` | Validate and route to the selected candidate. |
| Advisory | `NoDecision` | Run `AIBRIX_EXTERNAL_ROUTER_FALLBACK`. |
| Advisory | `Denied` | Protocol failure; apply configured failure mode. |
| Authoritative | `Selected` | Validate and route to the selected candidate. |
| Authoritative | `Denied` | Return fixed HTTP 403; no fallback and no backend request. |
| Authoritative | `NoDecision` | Protocol failure and fixed HTTP 503. |

System failures include timeout, DNS/TLS/connection errors, non-200 status,
invalid media type or JSON, oversized bodies, invalid targets, bulkhead
saturation, and an open circuit breaker.

- `FailOpen` runs the configured local fallback with the same candidate list.
- `FailClosed` returns HTTP 503 with error code
  `external_router_unavailable`.
- `Authoritative` requires `FailClosed`.
- Caller cancellation propagates without fallback or a circuit-breaker failure.

## Gateway configuration

The included patch configures an Advisory deployment:

```yaml
- name: AIBRIX_EXTERNAL_ROUTER_ENDPOINT
  value: http://external-replica-router.aibrix-system.svc:8080/v1alpha1/select
- name: AIBRIX_EXTERNAL_ROUTER_POLICY_MODE
  value: Advisory
- name: AIBRIX_EXTERNAL_ROUTER_FAILURE_MODE
  value: FailOpen
- name: AIBRIX_EXTERNAL_ROUTER_FALLBACK
  value: least-request
- name: AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES
  value: topology.kubernetes.io/zone,routing.example.com/accelerator-class
- name: AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS
  value: runningRequests,engineUtilization,kvCacheUsage
```

All supported settings are process-level:

| Variable | Default | Purpose |
|---|---:|---|
| `AIBRIX_EXTERNAL_ROUTER_ENDPOINT` | unset | Complete static HTTP/HTTPS operation URL. Unset disables the strategy. |
| `AIBRIX_EXTERNAL_ROUTER_POLICY_MODE` | none | Required when enabled: `Advisory` or `Authoritative`. |
| `AIBRIX_EXTERNAL_ROUTER_FAILURE_MODE` | none | Required when enabled: `FailOpen` or `FailClosed`. |
| `AIBRIX_EXTERNAL_ROUTER_FALLBACK` | none | Registered local non-external, non-exclusive router; required for Advisory and FailOpen. `pd` and `slo*` are rejected. |
| `AIBRIX_EXTERNAL_ROUTER_TIMEOUT` | `10ms` | Positive deadline for one HTTP exchange. Set an explicit value appropriate for the deployment network. |
| `AIBRIX_EXTERNAL_ROUTER_MAX_INFLIGHT` | `256` | Non-blocking per-Gateway bulkhead capacity. |
| `AIBRIX_EXTERNAL_ROUTER_MAX_REQUEST_BYTES` | `256KiB` | Maximum encoded request size. |
| `AIBRIX_EXTERNAL_ROUTER_MAX_RESPONSE_BYTES` | `64KiB` | Maximum response size. |
| `AIBRIX_EXTERNAL_ROUTER_FAILURE_THRESHOLD` | `5` | Consecutive attempted-exchange failures before opening the circuit. |
| `AIBRIX_EXTERNAL_ROUTER_OPEN_DURATION` | `1s` | Open interval before one half-open probe is allowed. |
| `AIBRIX_EXTERNAL_ROUTER_AUTH_TOKEN_FILE` | unset | Optional readable file containing a non-empty bearer token, loaded once at startup. |
| `AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES` | empty | Comma-separated pod-label allowlist. |
| `AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS` | empty | Any of `runningRequests`, `engineUtilization`, `kvCacheUsage`. Metrics are not sent by default. |
| `AIBRIX_EXTERNAL_ROUTER_POLICY_ATTRIBUTES` | empty | Comma-separated allowlist for trusted attributes populated inside Gateway. |

Invalid enabled configuration fails Gateway initialization rather than leaving a
partially enabled strategy.

## Build and install

Replace the example registry with one accessible from the Kubernetes cluster:

```bash
docker build -t example.com/aibrix/external-replica-router:v1alpha1 \
  samples/external-replica-router/decision-service
docker push example.com/aibrix/external-replica-router:v1alpha1
kubectl apply -k samples/external-replica-router
kubectl patch deployment aibrix-gateway-plugins -n aibrix-system \
  --type strategic \
  --patch-file samples/external-replica-router/manifests/gateway-plugin-patch.yaml
kubectl rollout status deployment/external-replica-router -n aibrix-system
kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system
```

The sample model image is `aibrix/vllm-mock:nightly`. Replace it with the real
model deployment when adapting this example.

Send an inference request through Gateway:

```bash
curl -i http://localhost:8888/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer token' \
  -H 'routing-strategy: external' \
  -d '{
    "model": "premium-model",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

On success, inspect the `target-pod` response header. Gateway selects the pod
address locally after validating the service's response.

## Exercise the protocol directly

For local development only, port-forward the sample decision service:

```bash
kubectl -n aibrix-system port-forward service/external-replica-router 18080:8080
```

Then send a protocol request. The operation requires the exact versioned media
type in both `Content-Type` and `Accept` when called by Gateway:

```bash
curl -sS http://127.0.0.1:18080/v1alpha1/select \
  -H 'Content-Type: application/vnd.aibrix.external-routing+json;version=v1alpha1' \
  -H 'Accept: application/vnd.aibrix.external-routing+json;version=v1alpha1' \
  -H 'X-Request-Id: req-123' \
  --data-binary @- <<'JSON'
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionRequest",
  "metadata": {"requestId": "req-123"},
  "spec": {
    "model": "premium-model",
    "policyMode": "Advisory",
    "candidates": [
      {
        "id": "default/premium-model-h100-a",
        "ports": [8000],
        "attributes": {
          "topology.kubernetes.io/zone": "cn-east-1a",
          "routing.example.com/accelerator-class": "h100"
        },
        "metrics": {"runningRequests": 2}
      }
    ]
  }
}
JSON
```

The sample returns:

```json
{
  "apiVersion": "routing.aibrix.ai/v1alpha1",
  "kind": "ReplicaSelectionResponse",
  "metadata": {"requestId": "req-123"},
  "status": {
    "decision": "Selected",
    "target": {
      "id": "default/premium-model-h100-a",
      "port": 8000
    }
  }
}
```

## Adapt the policy

To route `premium-model` to H100 replicas in `cn-east-1a`:

1. Label model pods with
   `routing.example.com/accelerator-class=h100` and
   `topology.kubernetes.io/zone=cn-east-1a`.
2. Put exactly those keys in
   `AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES`. Labels outside the allowlist
   never leave Gateway.
3. Update the named predicate in `decide`; use only the declared protocol and
   operator-owned policy data.
4. Add table cases for a match, no match, missing attributes, malformed input,
   and deterministic tie-breaking.
5. Rebuild and deploy the decision-service image.
6. Use `NoDecision` to exercise Advisory fallback. Use `Denied` only in a
   separately tested Authoritative + FailClosed deployment.
7. Inspect Gateway metrics and bounded error logs during rollout.

Trusted policy attributes are deployment-specific. Populate them only from
validated in-process identity or configuration; never copy raw tenant,
authorization, or arbitrary client headers.

## Observability

Gateway exposes the following low-cardinality metrics:

- `aibrix_gateway_external_router_requests_total{outcome}`
- `aibrix_gateway_external_router_fallback_total{reason}`
- `aibrix_gateway_external_router_duration_seconds`
- `aibrix_gateway_external_router_inflight`
- `aibrix_gateway_external_router_circuit_state{state}`

The fallback counter increments when Gateway invokes the local fallback, even
when that fallback returns an error.

Endpoint, model, pod, request ID, decision ID, external reason, status code, and
attribute values are deliberately not Prometheus labels.

## Validate the sample

```bash
python samples/external-replica-router/decision-service/test_router.py
kubectl kustomize samples/external-replica-router >/dev/null
kubectl apply --dry-run=client -k samples/external-replica-router
kubectl patch deployment aibrix-gateway-plugins -n aibrix-system \
  --type strategic --dry-run=client \
  --patch-file samples/external-replica-router/manifests/gateway-plugin-patch.yaml
```

The normative schema and additional conformance examples are published in
[`external-replica-selection-v1alpha1.yaml`](../../docs/source/_static/openapi/external-replica-selection-v1alpha1.yaml).
