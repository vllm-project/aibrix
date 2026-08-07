#!/bin/bash
set -x

export KUBECONFIG=${KUBECONFIG}
export aibrix_repo=${aibrix_repo}
export api_key=${api_key}
export kube_context=${kube_context}

workload_path=$1
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
    echo "The stdout/stderr is being logged in output.txt"
    ./run-test.sh ${workload_path} ${autoscaler} ${aibrix_repo} ${api_key} ${kube_context} > output.txt  2>&1
    end_time=$(date +%s)
    echo "Done: Time taken: $((end_time-start_time)) seconds"
    echo "--------------------------------"
    sleep 10
done
python plot-everything.py experiment_results/ 