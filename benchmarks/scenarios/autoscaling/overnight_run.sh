#!/bin/bash

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
    echo "The stdout/stderr is being logged in ./output.txt"
    ./run-test.sh ${workload_path} ${autoscaler} &> output.txt
    end_time=$(date +%s)
    echo "Done: Time taken: $((end_time-start_time)) seconds"
    echo "--------------------------------"
    sleep 10
done