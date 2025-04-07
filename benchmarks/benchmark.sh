#!/bin/bash
set -e

# ---------------
# CONFIGURATION
# ---------------

# Load config.sh if available
CONFIG_FILE="config.sh"

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
DATASET_FILE="${DATASET_DIR}/synthetic_dataset.jsonl"
WORKLOAD_FILE="${WORKLOAD_DIR}/workload.jsonl"

# ---------------
# STEP 1: DATASET GENERATION
# ---------------

generate_dataset() {
    echo "[INFO] Generating synthetic dataset..."
    python synthetic_prefix_sharing_dataset.py \
        --app-name programming \
        --prompt-length "$PROMPT_LENGTH" \
        --prompt-length-std "$PROMPT_STD" \
        --shared-proportion "$SHARED_PROP" \
        --shared-proportion-std "$SHARED_PROP_STD" \
        --num-samples-per-prefix "$NUM_SAMPLES" \
        --num-prefix "$NUM_PREFIX" \
        --randomize-order \
        > "$DATASET_FILE"
}

# ---------------
# STEP 2: WORKLOAD GENERATION
# ---------------

generate_workload() {
    echo "[INFO] Generating workload..."
    case "$WORKLOAD_TYPE" in
        constant)
            python workload_generator.py \
                --prompt-file "$DATASET_FILE" \
                --interval-ms "$INTERVAL_MS" \
                --duration-ms "$DURATION_MS" \
                --target-qps 1 \
                --trace-type constant \
                --model "$MODEL_NAME" \
                --output-dir "$WORKLOAD_DIR" \
                --output-format jsonl
            ;;
        synthetic)
            python workload_generator.py \
                --prompt-file "$DATASET_FILE" \
                --interval-ms "$INTERVAL_MS" \
                --duration-ms "$DURATION_MS" \
                --trace-type synthetic \
                --traffic-pattern slight_fluctuation \
                --prompt-len-pattern slight_fluctuation \
                --completion-len-pattern slight_fluctuation \
                --model "$MODEL_NAME" \
                --output-dir "$WORKLOAD_DIR" \
                --output-format jsonl
            ;;
        azure)
            AZURE_TRACE="/tmp/AzureLLMInferenceTrace_conv.csv"
            wget https://raw.githubusercontent.com/Azure/AzurePublicDataset/refs/heads/master/data/AzureLLMInferenceTrace_conv.csv -O "$AZURE_TRACE"
            python workload_generator.py \
                --prompt-file "$DATASET_FILE" \
                --interval-ms "$INTERVAL_MS" \
                --duration-ms "$DURATION_MS" \
                --trace-type azure \
                --trace-file "$AZURE_TRACE" \
                --group-interval-seconds 1 \
                --model "$MODEL_NAME" \
                --output-dir "$WORKLOAD_DIR"
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
generate_dataset
generate_workload
run_client
run_analysis
echo "========== Benchmark Completed =========="

