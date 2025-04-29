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