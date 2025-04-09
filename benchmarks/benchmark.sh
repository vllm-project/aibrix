#!/bin/bash
set -e
set -x

# ---------------
# CONFIGURATION
# ---------------

# Load config.sh if available
CONFIG_FILE="config/base.sh"

if [[ -f "$CONFIG_FILE" ]]; then
    echo "[INFO] Loading configuration from $CONFIG_FILE"
    source "$CONFIG_FILE"
else
    echo "[ERROR] config.sh not found. Please create one based on config.sh.example"
    exit 1
fi

# Create derived paths
mkdir -p "$DATASET_DIR" "$WORKLOAD_DIR" "$CLIENT_OUTPUT" "$TRACE_OUTPUT"

# Dataset generation config
# DATASET_FILE="${DATASET_DIR}/synthetic_dataset.jsonl"
WORKLOAD_FILE="${WORKLOAD_DIR}/workload.jsonl"

# ---------------
# STEP 1: DATASET GENERATION
# ---------------

generate_dataset() {
    echo "[INFO] Generating synthetic dataset..."

    case "$PROMPT_TYPE" in
        synthetic_shared)
            python generator/dataset-generator/synthetic_prefix_sharing_dataset.py \
                --app-name "$PROMPT_TYPE" \
                --prompt-length "$PROMPT_LENGTH" \
                --prompt-length-std "$PROMPT_STD" \
                --shared-proportion "$SHARED_PROP" \
                --shared-proportion-std "$SHARED_PROP_STD" \
                --num-samples-per-prefix "$NUM_SAMPLES" \
                --num-prefix "$NUM_PREFIX" \
                --output "$DATASET_FILE" \
                --randomize-order \
            ;;
        synthetic_multiturn)
            python generator/dataset-generator/multiturn_prefix_sharing_dataset.py \
                --prompt-length-mean "$PROMPT_LENGTH" \
                --prompt-length-std "$PROMPT_STD" \
                --num-turns-mean "$NUM_TURNS" \
                --num-turns-std "$NUM_TURNS_STD" \
                --num-sessions-mean "$NUM_SESSIONS" \
                --num-sessions-std "$NUM_SESSIONS_STD" \
                --output "$DATASET_FILE" \
            ;;
        client_trace)
            python generator/dataset-generator/converter.py \
                --path ${TRACE} \
                --type trace \
                --output ${DATASET_FILE} \
            ;;
        sharegpt)
            if [[ ! -f "$TARGET_DATASET" ]]; then
                echo "[INFO] Downloading ShareGPT dataset..."
                wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json -O ${TARGET_DATASET}
            fi    
            python generator/dataset-generator/converter.py  \
                --path ${TARGET_DATASET} \
                --type sharegpt \
                --output ${DATASET_FILE} \
            ;;
        *)
            echo "[ERROR] Unknown prompt type: $PROMPT_TYPE"
            exit 1
            ;;
    esac

}

# ---------------
# STEP 2: WORKLOAD GENERATION
# ---------------

generate_workload() {
    echo "[INFO] Generating workload..."
    case "$WORKLOAD_TYPE" in
        constant)
            python generator/workload-generator/workload_generator.py \
                --prompt-file "$DATASET_FILE" \
                --interval-ms "$INTERVAL_MS" \
                --duration-ms "$DURATION_MS" \
                --target-qps 1 \
                --trace-type constant \
                --model "$MODEL_NAME" \
                --target-qps "$TARGET_QPS" \
                --output-dir "$WORKLOAD_DIR" \
                --output-format jsonl
            ;;
        synthetic)
            CMD="python generator/workload-generator/workload_generator.py \
                --prompt-file \"$DATASET_FILE\" \
                --interval-ms \"$INTERVAL_MS\" \
                --duration-ms \"$DURATION_MS\" \
                --trace-type synthetic \
                --model \"$MODEL_NAME\" \
                --output-dir \"$WORKLOAD_DIR\" \
                --output-format jsonl"

            # Conditionally add optional arguments
            [ -n "$TRAFFIC_FILE" ] && CMD+=" --traffic-pattern-config \"$TRAFFIC_FILE\""
            [ -n "$PROMPT_LEN_FILE" ] && CMD+=" --prompt-len-pattern-config \"$PROMPT_LEN_FILE\""
            [ -n "$COMPLETION_LEN_FILE" ] && CMD+=" --completion-len-pattern-config \"$COMPLETION_LEN_FILE\""

            # Run the final command
            eval $CMD
            ;;
        azure)
            AZURE_TRACE="/tmp/AzureLLMInferenceTrace_conv.csv"
            wget https://raw.githubusercontent.com/Azure/AzurePublicDataset/refs/heads/master/data/AzureLLMInferenceTrace_conv.csv -O "$AZURE_TRACE"
            python generator/workload-generator/workload_generator.py \
                --prompt-file "$DATASET_FILE" \
                --interval-ms "$INTERVAL_MS" \
                --duration-ms "$DURATION_MS" \
                --trace-type azure \
                --trace-file "$AZURE_TRACE" \
                --group-interval-seconds 1 \
                --model "$MODEL_NAME" \
                --output-dir "$WORKLOAD_DIR" \
                --output-format jsonl
            ;;
        *)
            echo "[ERROR] Unsupported workload type: $WORKLOAD_TYPE"
            exit 1
            ;;
    esac
}

# ---------------
# STEP 3: CLIENT DISPATCH
# ---------------

run_client() {
    echo "[INFO] Running client to dispatch workload..."
    python3 client.py \
        --workload-path "$WORKLOAD_FILE" \
        --endpoint "$ENDPOINT" \
        --model "$MODEL_PATH" \
        --api-key "$API_KEY" \
        --streaming \
        --output-file-path "$CLIENT_OUTPUT/output.jsonl"
}

# ---------------
# OPTIONAL: ANALYSIS
# ---------------

run_analysis() {
    echo "[INFO] Analyzing trace output..."
    python analyze.py \
        --trace "$CLIENT_OUTPUT/output.jsonl" \
        --output "$TRACE_OUTPUT" \
        --goodput-target "$GOODPUT_TARGET" 
}

# ---------------
# MAIN
# ---------------

echo "========== Starting Benchmark =========="
# generate_dataset
generate_workload
# run_client
# run_analysis
echo "========== Benchmark Completed =========="

