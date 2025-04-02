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
        if pod.running_request < mean + std_dev {
            return pod
        }
    }
}

// selects pod with minimum running requests, similar to least-request routing algorithm
func select_pod_with_least_running_requests(ready_pods) {
    return select_pod_min_running_requests()
}
```