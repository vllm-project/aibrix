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

# ---------------
# STEP 1: DATASET GENERATION
# ---------------

generate_dataset() {
    echo "[INFO] Generating synthetic dataset ${PROMPT_TYPE}..."

    case "$PROMPT_TYPE" in
        synthetic_shared)
            source config/dataset/synthetic_shared.sh
            CMD="python generator/dataset-generator/synthetic_prefix_sharing_dataset.py \
                --tokenizer "$TOKENIZER" \
                --app-name \"$PROMPT_TYPE\" \
                --prompt-length \"$PROMPT_LENGTH\" \
                --prompt-length-std \"$PROMPT_STD\" \
                --shared-proportion \"$SHARED_PROP\" \
                --shared-proportion-std \"$SHARED_PROP_STD\" \
                --num-samples-per-prefix \"$NUM_SAMPLES\" \
                --num-prefix \"$NUM_PREFIX\" \
                --output \"$DATASET_FILE\" \
                --randomize-order"

            [ -n "$NUM_DATASET_CONFIGS" ] && CMD+=" --num-configs \"$NUM_DATASET_CONFIGS\""
            
            eval $CMD
            ;;
        synthetic_multiturn)
            source config/dataset/synthetic_multiturn.sh
            python generator/dataset-generator/multiturn_prefix_sharing_dataset.py \
                --tokenizer "$TOKENIZER" \
                --shared-prefix-len "$SHARED_PREFIX_LENGTH" \
                --prompt-length-mean "$PROMPT_LENGTH" \
                --prompt-length-std "$PROMPT_STD" \
                --num-turns-mean "$NUM_TURNS" \
                --num-turns-std "$NUM_TURNS_STD" \
                --num-sessions-mean "$NUM_SESSIONS" \
                --num-sessions-std "$NUM_SESSIONS_STD" \
                --output "$DATASET_FILE" \
            ;;
        client_trace)
            source config/dataset/client_trace.sh
            python generator/dataset-generator/utility.py convert \
                --path ${TRACE} \
                --type trace \
                --output ${DATASET_FILE} \
            ;;
        sharegpt)
            source config/dataset/sharegpt.sh
            if [[ ! -f "$TARGET_DATASET" ]]; then
                echo "[INFO] Downloading ShareGPT dataset..."
                wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json -O ${TARGET_DATASET}
            fi    
            python generator/dataset-generator/utility.py convert \
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
            source config/workload/constant.sh
            CMD="python generator/workload-generator/workload_generator.py \
                --prompt-file \"$DATASET_FILE\" \
                --interval-ms \"$INTERVAL_MS\" \
                --duration-ms \"$DURATION_MS\" \
                --target-qps \"$TARGET_QPS\" \
                --trace-type constant \
                --model \"$MODEL_NAME\" \
                --output-dir \"$WORKLOAD_DIR\" \
                --output-format jsonl"

            [ -n "$MAX_CONCURRENT_SESSIONS" ] && CMD+=" --max-concurrent-sessions \"$MAX_CONCURRENT_SESSIONS\""

            eval $CMD
            ;;
        synthetic)
            source config/workload/synthetic.sh
            CMD="python generator/workload-generator/workload_generator.py \
                --prompt-file \"$DATASET_FILE\" \
                --interval-ms \"$INTERVAL_MS\" \
                --duration-ms \"$DURATION_MS\" \
                --trace-type synthetic \
                --model \"$MODEL_NAME\" \
                --output-dir \"$WORKLOAD_DIR\" \
                --output-format jsonl"

            [ -n "$TRAFFIC_FILE" ] && CMD+=" --traffic-pattern-config \"$TRAFFIC_FILE\""
            [ -n "$PROMPT_LEN_FILE" ] && CMD+=" --prompt-len-pattern-config \"$PROMPT_LEN_FILE\""
            [ -n "$COMPLETION_LEN_FILE" ] && CMD+=" --completion-len-pattern-config \"$COMPLETION_LEN_FILE\""
            [ -n "$MAX_CONCURRENT_SESSIONS" ] && CMD+=" --max-concurrent-sessions \"$MAX_CONCURRENT_SESSIONS\""

            eval $CMD
            ;;
        stat)            
            source config/workload/stat.sh
            CMD="python generator/workload-generator/workload_generator.py \
                --prompt-file \"$DATASET_FILE\" \
                --interval-ms \"$INTERVAL_MS\" \
                --duration-ms \"$DURATION_MS\" \
                --trace-type stat \
                --stat-trace-type \"$STAT_TRACE_TYPE\" \
                --model \"$MODEL_NAME\" \
                --traffic-file \"$TRAFFIC_FILE\" \
                --prompt-len-file \"$PROMPT_LEN_FILE\" \
                --completion-len-file \"$COMPLETION_LEN_FILE\" \
                --output-dir \"$WORKLOAD_DIR\" \
                --output-format jsonl"

            [ -n "$QPS_SCALE" ] && CMD+=" --qps-scale \"$QPS_SCALE\""
            [ -n "$OUTPUT_SCALE" ] && CMD+=" --output-scale \"$OUTPUT_SCALE\""
            [ -n "$INPUT_SCALE" ] && CMD+=" --input-scale \"$INPUT_SCALE\""
            eval $CMD
            ;;
        azure)
            source config/workload/azure.sh
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

    CMD="python client/client.py \
        --workload-path \"$WORKLOAD_FILE\" \
        --endpoint \"$ENDPOINT\" \
        --model \"$TARGET_MODEL\" \
        --api-key \"$API_KEY\" \
        --time-scale \"$TIME_SCALE\" \
        --output-file-path \"$CLIENT_OUTPUT/output.jsonl\""

    [ "$STREAMING_ENABLED" = "true" ] && CMD+=" --streaming"
    [ -n "$CLIENT_POOL_SIZE" ] && CMD+=" --client-pool-size \"$CLIENT_POOL_SIZE\""
    [ -n "$OUTPUT_TOKEN_LIMIT" ] && CMD+=" --output-token-limit \"$OUTPUT_TOKEN_LIMIT\""
    eval $CMD
}

# ---------------
# OPTIONAL: ANALYSIS
# ---------------

run_analysis() {
    echo "[INFO] Analyzing trace output..."
    python client/analyze.py \
        --trace "$CLIENT_OUTPUT/output.jsonl" \
        --output "$TRACE_OUTPUT" \
        --goodput-target "$GOODPUT_TARGET" 
}

# ---------------
# MAIN
# ---------------

echo "========== Starting Benchmark =========="
COMMAND="$1"

case "$COMMAND" in
  dataset)
    generate_dataset
    ;;
  workload)
    generate_workload
    ;;
  client)
    run_client
    ;;
  analysis)
    run_analysis
    ;;
  all|"")
    generate_dataset
    generate_workload
    run_client
    run_analysis
    ;;
  *)
    echo "[ERROR] Unknown command: $COMMAND"
    echo "Usage: $0 [dataset|workload|client|analysis|all]"
    exit 1
    ;;
esac
echo "========== Benchmrk Completed =========="

