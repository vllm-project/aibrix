#!/bin/bash
set -x
#./run.sh workload/workload/25min_up_and_down/25min_up_and_down.jsonl

export KUBECONFIG=~/.kube/config-vke
export aibrix_repo="/root/aibrix-local"
export api_key="sk-kFJ12nKsFVfVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BlbLi"
export kube_context="ccr3aths9g2gqedu8asdg@35122069-kcu0n2lfb7pjdd83330h0"

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
        echo "The stdout/stderr is being logged in output-${WORKLOAD_TYPE}.txt"
        ./run-test.sh ${workload_path} ${autoscaler} ${aibrix_repo} ${api_key} ${kube_context} ${WORKLOAD_TYPE} > output-${WORKLOAD_TYPE}.txt  2>&1
        end_time=$(date +%s)
        echo "Done: Time taken: $((end_time-start_time)) seconds"
        echo "--------------------------------"
        sleep 10
    done
done

# for WORKLOAD_TYPE in "T_HighSlow_I_HighSlow_O_HighFast"  "T_HighSlow_I_HighSlow_O_HighSlow" "T_HighSlow_I_LowFast_O_HighSlow" "T_HighSlow_I_LowSlow_O_HighSlow"
# do
#     python plot-everything.py experiment_results/${WORKLOAD_TYPE}
# done
