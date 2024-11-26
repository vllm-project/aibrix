#!/bin/bash

python workload_generator.py --prompt-file $SHARE_GPT_PATH --num-prompts 100 --num-requests 10000 
python workload_generator.py --prompt-file $SHARE_GPT_PATH --num-prompts 100 --num-requests 100000 --trace-file "$TRACE_FILE"