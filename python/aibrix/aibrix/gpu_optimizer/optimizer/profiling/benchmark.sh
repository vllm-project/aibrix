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
TEMPERATURE=0.0  

TOTAL=100  # Set your preferred request sizes and rates here.
input_start=4
input_limit=$((2**12)) # 4K
output_start=4
output_limit=$((2**12)) # 4K
rate_start=1
rate_limit=$((2**6)) # 64
workload=
dry_run=0


# Function to generate workload for specific input/output lengths
generate_workload() {
    local input_len=$1
    local output_len=$2
    local api_key=$3
    local num_prompts=$4
    local model=$5
    local workload_path=$6
    local output_dir=$7

    local prompt_path
    prompt_path=$(python $PATH_PREFIX/gen_benchmark_prompt.py \
        $workload_path  \
        --input-tokens "$input_len" \
        --min-output-tokens "$output_len" \
        --tolerance "0.2" \
        --qps "2.0" \
        --host "localhost" \
        --port "8010" \
        --api-key "$api_key" \
        --total-prompts "$num_prompts" \
        --model "$model" \
        --temperature "$TEMPERATURE" \
        --output-dir "$output_dir" 2>&1 | tail -n 1)

    echo "$prompt_path"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--model)
      MODEL=$2
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
    --temperature)
      TEMPERATURE=$2
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


if [[ -z "$OUTPUT_FILE" ]]; then
  echo "Use default output path"
  OUTPUT_FILE="${PATH_PREFIX}/result/${MODEL}.jsonl"
fi

dir_path=$(dirname "$OUTPUT_FILE")
PROMPT_DIR="${dir_path}/prompts"

mkdir -p "$(dirname "$OUTPUT_FILE")"
mkdir -p "$PROMPT_DIR"

# Clear the workload directory
echo "Clearing workload directory: $PROMPT_DIR"
rm -rf "$PROMPT_DIR"/*

# Append Mode: Uncomment the below line if you want the output file to be empty every run
# > "$OUTPUT_FILE"

# Print the arguments (or use them in your script logic)
echo "Start benchmark $MODEL, input tokens:[$input_start:$input_limit], output tokens:[$output_start:$output_limit], rates:[$rate_start:$rate_limit], save as: $OUTPUT_FILE", workload: "$workload"


if [[ $dry_run == 1 ]]; then
  echo "Dry run enabled, skip profiling."
  exit
fi

# Run the benchmark for each combination
echo "Starting benchmark..."
input_len=$input_start
while [[ $input_len -le $input_limit ]]; do
  output_len=$output_start
  while [[ $output_len -le $output_limit ]]; do

    if [[ -n "$workload" ]]; then
      # Make sure all arguments are passed in the correct order
      WORKLOAD_FILE=$(generate_workload "$input_len" "$output_len" "$LLM_API_KEY" "$TOTAL" "$MODEL" "$workload" "$dir_path")
      echo "Workload file: $WORKLOAD_FILE"
    else
      echo "Skip workload pattern generation, benchmark with fixed prompts"
    fi
  
    # Convert rate_start to integer (multiply by 1000 and remove decimals since -le does not work with floats)
    req_rate=$(printf "%.0f\n" "$(echo "$rate_start * 1000" | awk '{print $1 * 1000}')")    
    rate_limit_scaled=$(printf "%.0f\n" "$(echo "$rate_limit * 1000" | awk '{print $1 * 1000}')")
    while [[ $req_rate -le $rate_limit_scaled ]]; do
      actual_rate=$(echo "$req_rate" | awk '{ printf "%.3f", $1 / 1000 }')
      if [[ -n "$workload" ]]; then
        if [[ -f "$WORKLOAD_FILE" ]]; then
            echo "run benchmark with workload file: $WORKLOAD_FILE"
            # If workload file exists, run the benchmark with $WORKLOAD_FILE
            python $PATH_PREFIX/gpu_benchmark.py --backend=vllm --port 8010 --model=$MODEL --request-rate=$actual_rate --num-prompts=$TOTAL --input-len $input_len --output-len $output_len --api-key "$LLM_API_KEY" --temperature "$TEMPERATURE" --workload_dataset_file "$WORKLOAD_FILE" --stream >> "$OUTPUT_FILE" 
        fi
        # If workload file does not exist, print the command to run the benchmark
      else
        echo "run benchmark with fixed prompts: input=$input_len, output=$output_len, rate=$actual_rate"
        python $PATH_PREFIX/gpu_benchmark.py --backend=vllm --port 8010 --model=$MODEL --request-rate=$actual_rate --num-prompts=$TOTAL --input-len $input_len --output-len $output_len --api-key "$LLM_API_KEY" --temperature "$TEMPERATURE" --stream >> "$OUTPUT_FILE" 
      fi
      req_rate=$((req_rate * 2)) 
    done
    output_len=$((output_len * 2)) 
  done
  input_len=$((input_len * 2))
done
echo "Benchmarking finished."