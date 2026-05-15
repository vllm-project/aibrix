# Virtual Token Counter (VTC) Routing

Virtual Token Counter (VTC) is a fair scheduling algorithm for LLM serving based on the paper
[Fairness in Serving Large Language Models (Sheng et al., 2024)](https://arxiv.org/abs/2401.00588).
VTC tracks the weighted token service each client has received and routes new requests to the pod
that gives the least-served client the best chance of progressing — balancing fairness with
cluster utilization.

## vtc-basic

`vtc-basic` is a Kubernetes-native simplification of VTC. Rather than managing a single global
queue, it selects a target pod per request by combining two scores:

- **Fairness score** — how much token service this client has received relative to all clients
  within a sliding time window. Clients who have received less service get a lower (better) score.
- **Utilization score** — the pod's current load normalized against `MAX_POD_LOAD`.

The pod with the **lowest combined score** is selected.

### Scoring diagram

```
For each ready pod:

  fairness_score   = client_tokens_in_window / total_tokens_in_window
                     (normalized, 0.0 → 1.0; lower = client received less service)

  utilization_score = pod_running_requests / MAX_POD_LOAD
                     (normalized, 0.0 → 1.0; lower = pod is less loaded)

  combined_score   = FAIRNESS_WEIGHT   × fairness_score
                   + UTILIZATION_WEIGHT × utilization_score

  ──────────────────────────────────────────────────────────────
  Select pod with minimum combined_score
```

### Request lifecycle

```
Incoming Request (with user identity)
        │
        ▼
┌──────────────────────────────────┐
│  Look up client token count      │
│  in sliding time window          │
└───────────────┬──────────────────┘
                │
                ▼
┌──────────────────────────────────┐
│  Compute fairness_score          │
│  per ready pod                   │
└───────────────┬──────────────────┘
                │
                ▼
┌──────────────────────────────────┐
│  Compute utilization_score       │
│  per ready pod                   │
└───────────────┬──────────────────┘
                │
                ▼
┌──────────────────────────────────┐
│  combined_score =                │
│    w_f × fairness                │
│  + w_u × utilization             │
└───────────────┬──────────────────┘
                │
                ▼
        Select min-score pod
                │
                ▼
┌──────────────────────────────────┐
│  After response: update client   │
│  token count in window           │
│  (input × w_i + output × w_o)   │
└──────────────────────────────────┘
```

End-to-end tests: [vtc_routing_test.go](../../../test/e2e/vtc_routing_test.go)

## Configuration

| Environment Variable                          | Description                                                                  | Default        |
|-----------------------------------------------|------------------------------------------------------------------------------|----------------|
| `AIBRIX_ROUTING_ALGORITHM`                    | Set to `vtc-basic` to enable this routing strategy.                          | `prefix-aware` |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT`   | Time unit for the sliding window. Options: `seconds`, `minutes`, `hours`.    | `minutes`      |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE` | Number of time units in the sliding window.                                  | `5`            |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS`  | Minimum token floor for adaptive tracking — prevents division by near-zero.  | `1000`         |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS`  | Maximum token cap for adaptive tracking.                                     | `8000`         |
| `AIBRIX_ROUTER_VTC_BASIC_INPUT_TOKEN_WEIGHT`  | Weight applied to input tokens when updating a client's service counter.     | `1.0`          |
| `AIBRIX_ROUTER_VTC_BASIC_OUTPUT_TOKEN_WEIGHT` | Weight applied to output tokens when updating a client's service counter.    | `2.0`          |
| `AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD`        | Normalization denominator for pod running-request count.                     | `100.0`        |
| `AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT`     | Multiplier for the fairness term in the combined score (`w_f`).              | `1.0`          |
| `AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT`  | Multiplier for the utilization term in the combined score (`w_u`).           | `1.0`          |

### Tuning guidance

- **Output token weight > input token weight** (default `2.0` vs `1.0`) reflects that output
  generation is more GPU-expensive than prompt processing.
- **Increase `FAIRNESS_WEIGHT`** relative to `UTILIZATION_WEIGHT` when strict per-client
  fairness matters more than raw throughput.
- **Increase `UTILIZATION_WEIGHT`** to prioritize routing to less-loaded pods, trading some
  fairness for better overall cluster efficiency.
- **Window size** controls how long past token usage influences routing. A shorter window (e.g.
  `1 minute`) makes the router more reactive; a longer window (e.g. `10 minutes`) smooths out
  bursty clients.

## Other VTC variants

The original VTC paper and reference implementation
([slora/server/router](https://github.com/Ying1123/VTC-artifact/tree/main/slora/server/router))
describe additional variants:

| Variant      | Description                                                                 |
|--------------|-----------------------------------------------------------------------------|
| `vtc-fair`   | Pure fairness — ignores utilization entirely.                               |
| `vtc-max-fair` | Maximizes minimum service across all clients.                             |
| `vtc-pred`   | Uses output-length prediction to improve fairness estimates.                |

Adapting these variants to the AIBrix pod-selection model (rather than a single shared queue)
is a candidate for future work.
