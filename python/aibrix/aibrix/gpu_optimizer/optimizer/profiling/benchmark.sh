#!/bin/bash

# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Result files will be added to 'PATH_PREFIX' directory.
PATH_PREFIX=`dirname "$0"`
OUTPUT_FILE=
MODEL="llama2-7b"
TOTAL=100
# TODO: Set your preferred request sizes and rates here.
input_start=4
input_limit=$((2**12)) # 4K
output_start=4
output_limit=$((2**12)) # 4K
rate_start=1
rate_limit=$((2**6)) # 64
workload=
dry_run=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--model)
      MODEL="$2"
      shift 2
      ;;
    -o|--output)
      OUTPUT_FILE="$2"
      shift 2
      ;;
    --input-start)
      input_start=$2
      shift 2
      ;;
    --input-limit)
      input_limit=$2
      shift 2
      ;;
    --output-start)
      output_start=$2
      shift 2
      ;;
    --output-limit)
      output_limit=$2
      shift 2
      ;;
    --rate-start)
      rate_start=$2
      shift 2
      ;;
    --rate-limit)
      rate_limit=$2
      shift 2
      ;;
    --dry-run)
      dry_run=1
      shift 1
      ;;
    --api-key)
      LLM_API_KEY=$2
      shift 2
      ;;
    --workload)
      workload="--workload_dataset_file $2"
      shift 2
      ;;
    # *)
    #   echo "Unknown option: $1"
    #   exit 1
    #   ;;
  esac
done

# Make sure the directory exists and clear output file
if [[ -z "$OUTPUT_FILE" ]]; then
  OUTPUT_FILE="${PATH_PREFIX}/result/${MODEL}.jsonl"
fi
mkdir -p `dirname "$OUTPUT_FILE"`

# Print the arguments (or use them in your script logic)
echo "Start benchmark $MODEL, input tokens:[$input_start:$input_limit], output tokens:[$output_start:$output_limit], rates:[$rate_start:$rate_limit], save as: $OUTPUT_FILE"
if [[ $dry_run == 1 ]]; then
  echo "Dru run enabled, skip profiling."
  exit
fi

input_len=$input_start
while [[ $input_len -le $input_limit ]]; do
  output_len=$output_start
  while [[ $output_len -le $output_limit ]]; do
    req_rate=$rate_start
    while [[ $req_rate -le $rate_limit ]]; do
      python $PATH_PREFIX/gpu_benchmark.py --backend=vllm --port 8010 --model=$MODEL --request-rate=$req_rate --num-prompts=$TOTAL --input-len $input_len --output-len $output_len --api-key "$LLM_API_KEY" --stream $workload 1>>${OUTPUT_FILE} 
      req_rate=$((req_rate * 2)) 
    done
    output_len=$((output_len * 2)) 
  done
  input_len=$((input_len * 2)) 
done

echo "Profiling finished."