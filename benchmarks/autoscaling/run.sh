#!/bin/bash
set -x

export KUBECONFIG=${KUBECONFIG}
export aibrix_repo=${aibrix_repo}
export api_key=${api_key}
export kube_context=${kube_context}

for WORKLOAD_TYPE in "T_HighSlow_I_HighSlow_O_HighFast"  "T_HighSlow_I_HighSlow_O_HighSlow" "T_HighSlow_I_LowFast_O_HighSlow" "T_HighSlow_I_LowSlow_O_HighSlow"
do
    workload_path="workload/synthetic_patterns/${WORKLOAD_TYPE}/synthetic_manual_config.jsonl"
    if [ -z "${workload_path}" ]; then
        echo "workload path is not given"
        echo "Usage: $0 <workload_path>"
        exit 1
    fi

    autoscalers="hpa kpa apa optimizer-kpa"
    for autoscaler in ${autoscalers}; do
        start_time=$(date +%s)
        echo "--------------------------------"
        echo "started experiment at $(date)"
        echo autoscaler: ${autoscaler}
        echo workload: ${workload_path} 
        echo "The stdout/stderr is being logged in output-${autoscaler}-${WORKLOAD_TYPE}.txt"
        ./run-test.sh ${workload_path} ${autoscaler} ${aibrix_repo} ${api_key} ${kube_context} ${WORKLOAD_TYPE} > output-${autoscaler}-${WORKLOAD_TYPE}.txt  2>&1
        end_time=$(date +%s)
        echo "Done: Time taken: $((end_time-start_time)) seconds"
        echo "--------------------------------"
        sleep 10
    done
    python plot-everything.py experiment_results/${WORKLOAD_TYPE} ${WORKLOAD_TYPE}
done




for WORKLOAD_TYPE in "workload-2024-10-10-19-50-00" "workload-2024-10-15-18-50-00" 
do
    workload_path="workload/maas/${WORKLOAD_TYPE}/internal.jsonl"
    if [ -z "${workload_path}" ]; then
        echo "workload path is not given"
        echo "Usage: $0 <workload_path>"
        exit 1
    fi

    autoscalers="hpa kpa apa optimizer-kpa"
    for autoscaler in ${autoscalers}; do
        start_time=$(date +%s)
        echo "--------------------------------"
        echo "started experiment at $(date)"
        echo autoscaler: ${autoscaler}
        echo workload: ${workload_path} 
        echo "The stdout/stderr is being logged in output-${WORKLOAD_TYPE}.txt"
        ./run-test.sh ${workload_path} ${autoscaler} ${aibrix_repo} ${api_key} ${kube_context} ${WORKLOAD_TYPE} > output-${WORKLOAD_TYPE}.txt  2>&1
        end_time=$(date +%s)
        echo "Done: Time taken: $((end_time-start_time)) seconds"
        echo "--------------------------------"
        sleep 10
    done
    python plot-everything.py experiment_results/${WORKLOAD_TYPE} ${WORKLOAD_TYPE}
done



# target_deployment="deepseek-llm-7b-chat"
# kubectl delete podautoscaler --all --all-namespaces
# python3 ${aibrix_repo}/benchmarks/utils/set_num_replicas.py --deployment ${target_deployment} --replicas 1 --context ${kube_context}
# target_ai_model=deepseek-llm-7b-chat


# mkdir -p output-profile
# for qps in {1..10} 
# do
#     kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &
#     STRATEGY="random"
#     WORKLOAD_PATH=workload/constant/qps-${qps}/constant.jsonl
#     python3 ${aibrix_repo}/benchmarks/client/client.py --workload-path ${WORKLOAD_PATH} --endpoint "http://localhost:8888" --model ${target_ai_model}  --api-key ${api_key} --output-file-path output-profile/output-qps${qps}.jsonl
#     # python analyze.py output-profile/output-qps${qps}.jsonl
#     sleep 30
# done