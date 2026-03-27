# Routing Algorithms

## Prefix Cache Aware

Below is the pseudo-code for prefix-cache aware routing.


```shell
func prefix_cache_routing(ready_pods []*v1.Pod) {
    if check_load_imbalance(ready_pods) {
        target_pod = select_pod_with_least_running_requests(ready_pods)
    } else {
        match_pods, prefix_hashes = match_prefix(ready_pods)
        if len(match_pod) > 0 {
            target_pod = select_least_loaded_match_pod(match_pods)
        }
    }

    // if no target pod is selected, fallback to select pod with least request
    if target_pod == nil {
        target_pod = select_pod_with_least_running_requests(ready_pods)
    }
}

func check_load_imbalance(ready_pods) {
    // filter pods with min and max number of running requests
    min_pod = select_pod_min_running_requests()
    max_pod = select_pod_max_running_requests()
    
    // if difference between max & min running requests count 
    // is more than configurable ABS_RUNNING_REQUEST_COUNT (default: 8)
    // then load is imbalanced
    if max_pod - min_pod > ABS_RUNNING_REQUEST_COUNT {
        return true
    }
    return false
}

func match_prefix(input_tokens, ready_pods) {
    // input_tokens are split based off configurable block_sizes and 
    // hash is calculated for each token_block
    hashes = calculate_hashes(input_tokens)

    // checks if token_block exists on ready_pods [prefix_match], 
    // if present calculate pod_name: prefix_match_percent
    match_pods_with_prefix_match_percent = check_hashes_on_ready_pods(hashes, ready_pods)
}

func select_least_loaded_match_pod(match_pods_with_prefix_match_percent, ready_pods) {
    mean = calculate_mean_running_request(ready_pods)   
    std_dev = calculate_std_dev_running_request(ready_pods)

    // sort match_pods in decreasing perfix_match_percent and 
    // for same prefix_match_percent, sort in increasing running_request count.
    sort(match_pods_with_prefix_match_percent)

    // select match pod with highest prefix and running_request < (mean + std_dev)
    for pod := range match_pods_with_prefix_match_percent {
        if pod.running_request < mean + load_factor*std_dev {
            return pod
        }
    }
}

// selects pod with minimum running requests, similar to least-request routing algorithm
func select_pod_with_least_running_requests(ready_pods) {
    return select_pod_min_running_requests()
}
```

## Configurations

- **_AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE_**

    AIBrix gateway implements two tokenizers **_character_** and **_tiktoken_**. Default tokenizer is <ins>**_character_**</ins>.
    
    | Tokenizer Type  | Details |
    | ------------- | ------------- |
    | character  | splits input text into characters  |
    | tiktoken  | open-source openai/tiktoken [tokenizer](https://github.com/openai/tiktoken)  |

- **_AIBRIX_PREFIX_CACHE_BLOCK_SIZE_**

    Tokenized input request is split into blocks and hash value of the blocks is cached for future match. Size of the block (i.e. number of tokens per block) defines how effective prefix match will be. Default is <ins>**_character tokenizer and 128 block size (tokens per block)_**</ins>.

    | Tokenizer Type  | Block Size Recommendation |
    | ------------- | ------------- |
    | character  | 128  |
    | tiktoken  | 16  |

- **AIBRIX_PREFIX_CACHE_BLOCK_NUMBER**

    Maximum number of prefix cache blocks. Default is <ins>**_200000_**</ins>.

- **AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT**

    Before evaluating prefix cache match, router checks if there is imbalance of running requests across pods. Imbalance is measured using absolute difference between max & min running requests across pods, for example if imbalance_abs_count = 16 and running requests for pods are [p1: 1, p2: 2, p3:20] then current scenario is flagged as imbalanced. If flagged as imbalanced then prefix match is ignored and request is routed to pod with least running requests which in above example will to route to pod p1. Default is <ins>**_16_**</ins> and should be adjusted based on GPU hardware & prompt length.

- **AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR**

    After evaluating prefix match, pods are selected with matching prefix cache. Selected pods are re-evaluated to prevent a hotspot scenario where bulk of prefix matching requests are routed to same pod. Imbalanced is checked as follows
    <pre>
    prefix_match_pod.running_requests <= mean + <b>load_factor</b> * standard_deviation
    </pre>

    **load_factor** determines number of standard deviations. Default is <ins>**_2_**</ins>

## Virtual Token Counter (VTC)

The Virtual Token Counter (VTC) is a fair scheduling algorithm for LLM serving based on the paper "Fairness in Serving Large Language Models" (Sheng et al.). VTC aims to provide fairness among clients by tracking the service (weighted token count) each client has received and prioritizing those who have received less service. It integrates with continuous batching and handles challenges unique to LLM serving, like variable token costs and unknown output lengths. The research paper and reference implementation artifact can be found at [Fairness in Serving Large Language Models (Sheng et al.)
](https://arxiv.org/abs/2401.00588).

### vtc-basic

The `vtc-basic` variant implements a simplified version of VTC. It routes requests by combining two scores: a fairness score based on the user’s token count (normalized against all users) within a time window, and a utilization score based on the pod’s current load. The pod with the lowest combined score is selected. This approach adapts dynamically to system load and balances fairness with efficient resource use.

End-to-end tests for this algorithm can be found in [vtc_routing_test.go](../../../test/e2e/vtc_routing_test.go).

#### Environment Variables

| Variable                                         | Description                                                                | Default          |
|--------------------------------------------------|----------------------------------------------------------------------------|------------------|
| `AIBRIX_ROUTING_ALGORITHM`                       | Set to `vtc-basic` to enable this routing strategy.                        | `prefix-aware`   |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT`      | Time unit of the sliding window for tracking user token usage.             | `minutes`        |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE`    | Size of the sliding window for tracking user token usage.                  | `5`              |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS`     | Sensible min default value for adaptive token tracking (see vtc_basic)     | `1000`           |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS`     | Sensible max default value for adaptive token tracking (see vtc_basic)     | `8000`           |
| `AIBRIX_ROUTER_VTC_BASIC_INPUT_TOKEN_WEIGHT`     | Weight applied to input tokens in fairness calculations.                   | `1.0`            |
| `AIBRIX_ROUTER_VTC_BASIC_OUTPUT_TOKEN_WEIGHT`    | Weight applied to output tokens in fairness calculations.                  | `2.0`            |
| `AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD`           | Normalization factor for pod load in utilization score calculation.        | `100.0`          |
| `AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT`        | Weight applied to fairness score in combined score calculation.            | `1.0`            |
| `AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT`     | Weight applied to utilization score in combined score calculation.         | `1.0`            |



### Other VTC variants
The VTC paper and reference implementation ([slora/server/router](https://github.com/Ying1123/VTC-artifact/tree/main/slora/server/router)) describe other VTC variants like `vtc-fair` (pure fairness), `vtc-max-fair`, and `vtc-pred` (using length prediction). Adapting these variants for use within the Aibrix Kubernetes routing context, which involves selecting specific pods rather than managing a single request queue, is a potential area for future enhancement.

## Prefill-Decode Disaggregation

Below is the pseudo-code for prefix-cache aware routing.


```shell
// Route take all ready pods for that model and later are filtered for Prefill and Decode.
func Route(readyPods *[]v1.Pod) {
    prefillPods, decodePods := filterPrefillAndDecodePods(readPods)

    doPrefillRequest(prefillPods)

    // randomly selects decode worker
    return selectDecode(decodePods)
}

func filterPrefillAndDecodePods(readyPods *[]v1.Pod) {
    prefillPods := filterBasedOfLabelSelector("role-name=prefill")
    decodePods := filterBasedOfLabelSelector("role-name=decode")

    return prefillPods, decodePods
}

// selects prefill worker and sends prefill request (based of engine)
func doPrefillRequest(prefillPods) {

    // check if any prefill pod already have shared prefix hence re-use kv-cache 
    // if no prefix sharing then use load-balancing to select prefill worker
    prefillPod := evaluatePrefixCache(prefillPods)

    // get inference engine using pod labels
    llmEngine := labelValue("model.aibrix.ai/engine")
    
    if llmEngine == "sglang" {
        async prefill_request() // for sglang send async prefill request
    } else {
        sync prefill_request()
    }

    return
}
```

- **AIBRIX_PREFILL_REQUEST_TIMEOUT**

    AIBrix gateway is an external processing plugin for envoy proxy which handles inference request validation, selection of target worker and more, but request forwarding to inference engine is done by envoy proxy.
    
    Prefill-Decode disaggregation is a special scenario, where prefill request is directly sent to inference engine by AIBrix gateway plugin and decode request is fowarded by envoy proxy. For prefill request if needed timeout can be configured using env variable, default is <ins>**_60s_**</ins>.

## Multi-Strategy Routing (Batch Soft Scoring)

AIBrix Gateway supports multi-strategy routing, allowing you to combine multiple routing algorithms with different weights. Instead of making hard decisions (where one strategy completely filters out pods or dictates the single winner), the multi-strategy router uses a **Batch Soft Scoring** mechanism to evaluate pods across all specified strategies.

### How it works

1. **Batch Scoring:** Each configured strategy evaluates all ready pods in a single batch operation (`ScoreAll`), returning raw scores and a boolean array indicating whether each pod could be scored (e.g., handling missing metrics).
2. **Normalization:** The raw scores from each strategy are normalized into a `[0, 1]` range:
   * **Higher is better** (e.g., `throughput`): `(score - min) / (max - min)`
   * **Lower is better** (e.g., `least-request`): `(max - score) / (max - min)`
   * If a pod lacks metrics (`scored=false`), its normalized score is `0.0` (acting as a penalty).
3. **Weighted Aggregation:** The normalized scores are multiplied by their respective configured weights and summed up for each pod.
4. **Top-1 Selection:** The pod with the highest total aggregated score is selected to serve the request.

### Configuration Format

You can configure multi-strategy routing by setting the `AIBRIX_ROUTING_ALGORITHM` environment variable using a comma-separated list of `strategy:weight` pairs.

* **Format:** `strategy1:weight1,strategy2:weight2,...`
* If `:weight` is omitted, it defaults to `1`.
* If a weight is `0`, the strategy is completely ignored.
* If an invalid strategy is specified, the system will fall back to the default `random` router.

### Examples

Here are some real-world examples demonstrating how the weighted aggregation works under different scenarios.

Assume we have two pods serving a model:
* **Pod A**: High load (10 requests) but High throughput (100 tok/s).
* **Pod B**: Low load (2 requests) but Low throughput (10 tok/s).

#### Example 1: `least-request` Dominates
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:10,throughput:1"
```
* `least-request` (weight 10): Pod B is much better (2 vs 10). Normalized: Pod A = 0.0, Pod B = 1.0. Weighted Score: A = 0.0 * 10/11 = 0.0, B = 1.0 * 10/11 ≈ 0.91.
* `throughput` (weight 1): Pod A is much better (100 vs 10). Normalized: Pod A = 1.0, Pod B = 0.0. Weighted Score: A = 1.0 * 1/11 ≈ 0.09, B = 0.0 * 1/11 = 0.0.
* **Total Score**: Pod A ≈ 0.09, Pod B ≈ 0.91. **Pod B wins!**

#### Example 2: `throughput` Dominates
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:1,throughput:10"
```
* `least-request` (weight 1): Weighted Score: A = 0.0 * 1/11 = 0.0, B = 1.0 * 1/11 ≈ 0.09.
* `throughput` (weight 10): Weighted Score: A = 1.0 * 10/11 ≈ 0.91, B = 0.0 * 10/11 = 0.0.
* **Total Score**: Pod A ≈ 0.91, Pod B ≈ 0.09. **Pod A wins!**

#### Example 3: Complex Scenario with Corner Cases (3 Strategies)
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request,throughput:2,least-latency:0"
```
* `least-request`: No weight specified, **defaults to 1**. Weighted Score: A = 0.0 * 1/3 = 0.0, B = 1.0 * 1/3 ≈ 0.33.
* `throughput`: Explicitly set to weight **2**. Weighted Score: A = 1.0 * 2/3 ≈ 0.67, B = 0.0 * 2/3 = 0.0.
* `least-latency`: Weight set to **0**, so this strategy is **ignored** and does not participate in scoring.
* **Total Score**: Pod A ≈ 0.67, Pod B ≈ 0.33. **Pod A wins!**

#### Example 4: Invalid Configuration Fallback
```yaml
AIBRIX_ROUTING_ALGORITHM: "least-request:1,invalid-strategy:1"
```
* Because `invalid-strategy` is not a registered algorithm, the multi-strategy parser will fail. The Gateway will gracefully fall back to using the default **Random Router** to ensure requests continue to be served without interruption.
