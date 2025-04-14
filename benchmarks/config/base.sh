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
export PROMPT_TYPE="synthetic_multiturn" #"sharegpt"
export DATASET_FILE="${DATASET_DIR}/${PROMPT_TYPE}.jsonl"

# ---------------
# STEP 2: WORKLOAD GENERATION
# ---------------
# Workload config
export WORKLOAD_TYPE="constant"  # Options: synthetic, constant, azure
export INTERVAL_MS=1000
export DURATION_MS=300000
export WORKLOAD_DIR="./output/workload/${WORKLOAD_TYPE}"


# ---------------
# STEP 3: CLIENT DISPATCH
# ---------------
# Client and trace analysis output directories
export WORKLOAD_FILE="./output/workload/constant/constant.jsonl"
export CLIENT_OUTPUT="./output/client_output"
export ENDPOINT="http://localhost:8000"
export API_KEY="$api_key"
export TARGET_MODEL="deepseek-llm-7b-chat"

# ---------------
# OPTIONAL: ANALYSIS
# ---------------
export TRACE_OUTPUT="./output/trace_analysis"
export GOODPUT_TARGET="tpot:0.5"
