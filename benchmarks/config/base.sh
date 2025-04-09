# config.sh
# DO NOT COMMIT this file if it contains sensitive info

# API and model settings

export MODEL_NAME="deepseek-ai/deepseek-llm-7b-chat"
export TOKENIZER="deepseek-ai/deepseek-llm-7b-chat"

# ---------------
# STEP 1: DATASET GENERATION
# -------
# Dataset config
export DATASET_DIR="./output/dataset/"
export PROMPT_TYPE="sharegpt"
export DATASET_FILE="${DATASET_DIR}/${PROMPT_TYPE}.jsonl"

## synthetic shared
# export PROMPT_TYPE="synthetic_shared"
export PROMPT_LENGTH=3871
export PROMPT_STD=1656
export SHARED_PROP=0.97
export SHARED_PROP_STD=0.074
export NUM_SAMPLES=200
export NUM_PREFIX=10


## synthetic_multiturn
# export PROMPT_TYPE="synthetic_multiturn"
export PROMPT_LENGTH=3871
export PROMPT_STD=1656
export NUM_TURNS=10
export NUM_TURNS_STD=1
export NUM_SESSIONS=10
export NUM_SESSIONS_STD=1


## client trace
# export PROMPT_TYPE="client_trace"
export TRACE="client_trace"

## sharegpt
# export PROMPT_TYPE="sharegpt"
export TARGET_DATASET="/tmp/ShareGPT_V3_unfiltered_cleaned_split.json"

# ---------------
# STEP 2: WORKLOAD GENERATION
# ---------------
# Workload config
export INTERVAL_MS=1000
export DURATION_MS=300000
export WORKLOAD_TYPE="synthetic"  # Options: synthetic, constant, azure
export WORKLOAD_DIR="./output/workloads"

# ---------------
# STEP 3: CLIENT DISPATCH
# ---------------
# Client and trace analysis output directories

export CLIENT_OUTPUT="./output/client_output"
export ENDPOINT="http://localhost:8000"
export API_KEY=$"api_key"
export MODEL_NAME="deepseek-llm-7b-chat"

# ---------------
# OPTIONAL: ANALYSIS
# ---------------
export TRACE_OUTPUT="./output/trace_analysis"
export GOODPUT_TARGET="topt:0.5"
