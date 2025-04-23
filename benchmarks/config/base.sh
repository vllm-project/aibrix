# config.sh
# DO NOT COMMIT this file if it contains sensitive info

# API and model settings

export MODEL_NAME="deepseek-ai/DeepSeek-R1-Distill-Llama-8B"
export TOKENIZER="deepseek-ai/DeepSeek-R1-Distill-Llama-8B"

# ---------------
# STEP 1: DATASET GENERATION
# -------
# Dataset config
export DATASET_DIR="./output/dataset/"
export PROMPT_TYPE="synthetic_multiturn" #"synthetic_multiturn", "synthetic_shared", "sharegpt", "client_trace"
export DATASET_FILE="${DATASET_DIR}/${PROMPT_TYPE}.jsonl"

# ---------------
# STEP 2: WORKLOAD GENERATION
# ---------------
# Workload config
export WORKLOAD_TYPE="constant"  # Options: constant, synthetic, constant, azure
export INTERVAL_MS=1000
export DURATION_MS=10000
export WORKLOAD_DIR="./output/workload/${WORKLOAD_TYPE}"


# ---------------
# STEP 3: CLIENT DISPATCH
# ---------------
# Client and trace analysis output directories
export WORKLOAD_FILE="${WORKLOAD_DIR}/workload.jsonl"
export CLIENT_OUTPUT="./output/client_output"
export ENDPOINT="http://localhost:8888"
export API_KEY="$api_key"
export TARGET_MODEL="llama-3-8b-instruct" #"deepseek-llm-7b-chat"
export TIME_SCALE="1.0" 
export ROUTING_STRATEGY="random" 
export STREAMING_ENABLED="true" # Options: true, false
export CLIENT_POOL_SIZE="128"
export OUTPUT_TOKEN_LIMIT="128"


# ---------------
# OPTIONAL: ANALYSIS
# ---------------
export TRACE_OUTPUT="./output/trace_analysis"
export GOODPUT_TARGET="tpot:0.5"
