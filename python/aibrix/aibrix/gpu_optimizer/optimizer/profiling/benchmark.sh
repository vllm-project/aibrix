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
FILE_NAME="result"
TOTAL=100

if [ -n "$1" ]; then
  # If first argument is provided, use the first argument as filename
  FILE_NAME="$1"
fi

# Make sure the directory exists and clear output file
OUTPUT_FILE="${PATH_PREFIX}/result/${FILE_NAME}.jsonl"
mkdir -p `dirname "$OUTPUT_FILE"`
# echo "" > ${OUTPUT_FILE}

# TODO: Set your preferred request sizes and rates here.
input_start=128
input_limit=$((2**11)) # 2K
output_start=4
output_limit=$((2**9)) # 512
rate_start=1
rate_limit=$((2**6)) # 32

input_len=$input_start
while [[ $input_len -le $input_limit ]]; do
  output_len=$output_start
  while [[ $output_len -le $output_limit ]]; do
    req_rate=$rate_start
    while [[ $req_rate -le $rate_limit ]]; do
      python $PATH_PREFIX/gpu_benchmark.py --backend=vllm --port 8010 --model=llama2-7b --request-rate=$req_rate --num-prompts=$TOTAL --input_len $input_len --output_len $output_len >> ${OUTPUT_FILE} 
      req_rate=$((req_rate * 2)) 
    done
    output_len=$((output_len * 2)) 
  done
  input_len=$((input_len * 2)) 
done

echo "Profiling finished."