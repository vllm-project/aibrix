# Multi-Strategy Routing (Batch Soft Scoring)

AIBrix Gateway supports multi-strategy routing, allowing you to combine multiple routing algorithms with different weights. Instead of making hard decisions (where one strategy completely filters out pods or dictates the single winner), the multi-strategy router uses a **Batch Soft Scoring** mechanism to evaluate pods across all specified strategies.

## How it works

1. **Batch Scoring:** Each configured strategy evaluates all ready pods in a single batch operation (`ScoreAll`), returning raw scores and a boolean array indicating whether each pod could be scored (e.g., handling missing metrics).
2. **Normalization:** The raw scores from each strategy are normalized into a `[0, 1]` range:
   * **Higher is better** (e.g., `throughput`): `(score - min) / (max - min)`
   * **Lower is better** (e.g., `least-request`): `(max - score) / (max - min)`
   * If a pod lacks metrics (`scored=false`), its normalized score is `0.0` (acting as a penalty).
3. **Weighted Aggregation:** The normalized scores are multiplied by their respective configured weights and summed up for each pod.
4. **Top-1 Selection:** The pod with the highest total aggregated score is selected to serve the request.

## Configuration Format

You can configure multi-strategy routing by setting the `AIBRIX_ROUTING_ALGORITHM` environment variable using a comma-separated list of `strategy:weight` pairs.

* **Format:** `strategy1:weight1,strategy2:weight2,...`
* If `:weight` is omitted, it defaults to `1`.
* If a weight is `0`, the strategy is completely ignored.
* If an invalid strategy is specified, the Gateway rejects the request with **400 Bad Request** instead of silently falling back to `random`.
* `session-affinity` is treated as a soft-scoring strategy in multi-strategy mode: it boosts the matching pod, but the final weighted winner may still be a different pod. The response session header is updated to the final selected pod.

## Examples

Here are some real-world examples demonstrating how the weighted aggregation works under different scenarios.

Assume we have two pods serving a model:
* **Pod A**: High load (10 requests) but High throughput (100 tok/s).
* **Pod B**: Low load (2 requests) but Low throughput (10 tok/s).

### Example 1: `least-request` Dominates
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:10,throughput:1"
```
* `least-request` (weight 10): Pod B is much better (2 vs 10). Normalized: Pod A = 0.0, Pod B = 1.0. Weighted Score: A = 0.0 * 10/11 = 0.0, B = 1.0 * 10/11 ≈ 0.91.
* `throughput` (weight 1): Pod A is much better (100 vs 10). Normalized: Pod A = 1.0, Pod B = 0.0. Weighted Score: A = 1.0 * 1/11 ≈ 0.09, B = 0.0 * 1/11 = 0.0.
* **Total Score**: Pod A ≈ 0.09, Pod B ≈ 0.91. **Pod B wins!**

### Example 2: `throughput` Dominates
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:1,throughput:10"
```
* `least-request` (weight 1): Weighted Score: A = 0.0 * 1/11 = 0.0, B = 1.0 * 1/11 ≈ 0.09.
* `throughput` (weight 10): Weighted Score: A = 1.0 * 10/11 ≈ 0.91, B = 0.0 * 10/11 = 0.0.
* **Total Score**: Pod A ≈ 0.91, Pod B ≈ 0.09. **Pod A wins!**

### Example 3: Complex Scenario with Corner Cases (3 Strategies)
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request,throughput:2,least-latency:0"
```
* `least-request`: No weight specified, **defaults to 1**. Weighted Score: A = 0.0 * 1/3 = 0.0, B = 1.0 * 1/3 ≈ 0.33.
* `throughput`: Explicitly set to weight **2**. Weighted Score: A = 1.0 * 2/3 ≈ 0.67, B = 0.0 * 2/3 = 0.0.
* `least-latency`: Weight set to **0**, so this strategy is **ignored** and does not participate in scoring.
* **Total Score**: Pod A ≈ 0.67, Pod B ≈ 0.33. **Pod A wins!**

### Example 4: Invalid Configuration Fallback
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:1,invalid-strategy:1"
```
* Because `invalid-strategy` is not a registered algorithm, the multi-strategy parser will fail. To preserve the API contract, the Gateway will reject the request with a **400 Bad Request** error rather than silently falling back to a random router.

### Example 5: Multi-Strategy Finding the Sweet Spot
In reality, a pod with the absolute minimum active requests might have just recovered from being severely overloaded. Multi-strategy routing prevents routing to extreme outliers by considering multiple dimensions.

Assume three pods with the following metrics:
* **Pod A**: Very low active requests (1 req), but historically massively overloaded (1500 tokens).
* **Pod B**: Historically barely used (15 tokens), but currently slammed with active requests (50 req).
* **Pod C**: The "Sweet Spot" - Medium active requests (10 req), and medium historical load (150 tokens).

**Single Strategy Flaws:**
* `AIBRIX_ROUTING_ALGORITHM="least-request"`: Completely ignores historical load and picks **Pod A** (which is exhausted).
* `AIBRIX_ROUTING_ALGORITHM="throughput"`: Completely ignores current spikes and picks **Pod B** (which is currently slammed).

**Multi-Strategy Solution:**
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:1,throughput:1"
```
* `least-request` (weight 1): Normalizes active requests [1, 50]. 
  * Pod A: 1.0
  * Pod B: 0.0
  * Pod C: 0.816
* `throughput` (weight 1): Normalizes historical load [15, 1500].
  * Pod A: 0.0
  * Pod B: 1.0
  * Pod C: 0.909
* **Total Score (Weighted)**: Pod A = 0.5, Pod B = 0.5, Pod C ≈ **0.86**. 
* **Result**: **Pod C wins!** The router correctly identifies the most balanced pod, avoiding the exhausted Pod A and the currently slammed Pod B.
