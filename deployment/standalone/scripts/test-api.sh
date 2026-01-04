#!/bin/bash
# ==============================================================================
# AIBrix Docker Compose - API Test Script
# ==============================================================================
# Usage:
#   ./scripts/test-api.sh              # Run all tests
#   ./scripts/test-api.sh --quick      # Quick health check only
#   ./scripts/test-api.sh --streaming  # Test streaming
# ==============================================================================

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

# Source .env if exists
if [ -f .env ]; then
    set -a
    source .env
    set +a
fi

# Defaults
BASE_URL="http://localhost:${HTTP_PORT:-80}"
MODEL_NAME="${MODEL_NAME:-meta-llama/Llama-3.1-8B-Instruct}"
QUICK_MODE=false
STREAMING_TEST=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --quick|-q)
            QUICK_MODE=true
            shift
            ;;
        --streaming|-s)
            STREAMING_TEST=true
            shift
            ;;
        --url)
            BASE_URL="$2"
            shift 2
            ;;
        --model)
            MODEL_NAME="$2"
            shift 2
            ;;
        -h|--help)
            echo "AIBrix API Test Script"
            echo ""
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --quick, -q       Quick health check only"
            echo "  --streaming, -s   Test streaming responses"
            echo "  --url URL         Base URL (default: http://localhost:80)"
            echo "  --model MODEL     Model name to test"
            echo "  -h, --help        Show this help"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║                    AIBrix API Tests                              ║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "Base URL: $BASE_URL"
echo "Model:    $MODEL_NAME"
echo ""

PASSED=0
FAILED=0

test_endpoint() {
    local name=$1
    local method=$2
    local endpoint=$3
    local data=$4
    local expected_status=${5:-200}

    printf "Testing %-30s ... " "$name"

    local response
    local status

    if [ "$method" = "GET" ]; then
        response=$(curl -s -w "\n%{http_code}" "$BASE_URL$endpoint" 2>/dev/null)
    else
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Content-Type: application/json" \
            -d "$data" \
            "$BASE_URL$endpoint" 2>/dev/null)
    fi

    status=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')

    if [ "$status" = "$expected_status" ]; then
        echo -e "${GREEN}PASS${NC} (HTTP $status)"
        ((PASSED++))
        return 0
    else
        echo -e "${RED}FAIL${NC} (HTTP $status, expected $expected_status)"
        if [ -n "$body" ]; then
            echo "    Response: $(echo "$body" | head -c 200)"
        fi
        ((FAILED++))
        return 1
    fi
}

test_streaming() {
    local name=$1
    local endpoint=$2
    local data=$3

    printf "Testing %-30s ... " "$name"

    local first_chunk
    first_chunk=$(curl -s -N -X POST \
        -H "Content-Type: application/json" \
        -d "$data" \
        "$BASE_URL$endpoint" 2>/dev/null | head -n1)

    if echo "$first_chunk" | grep -q "data:"; then
        echo -e "${GREEN}PASS${NC} (streaming)"
        ((PASSED++))
        return 0
    else
        echo -e "${RED}FAIL${NC} (no streaming data)"
        ((FAILED++))
        return 1
    fi
}

# ==============================================================================
# Health Checks
# ==============================================================================

echo -e "${CYAN}Health Checks${NC}"
echo "─────────────────────────────────────────────────────────────────────"

test_endpoint "Envoy Health" "GET" "/healthz"
test_endpoint "vLLM Health" "GET" "/health"

if [ "$QUICK_MODE" = true ]; then
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo -e "Results: ${GREEN}$PASSED passed${NC}, ${RED}$FAILED failed${NC}"
    exit $FAILED
fi

# ==============================================================================
# Model API
# ==============================================================================

echo ""
echo -e "${CYAN}Model API${NC}"
echo "─────────────────────────────────────────────────────────────────────"

test_endpoint "List Models" "GET" "/v1/models"

# ==============================================================================
# Chat Completions
# ==============================================================================

echo ""
echo -e "${CYAN}Chat Completions API${NC}"
echo "─────────────────────────────────────────────────────────────────────"

# Non-streaming chat
CHAT_REQUEST='{
  "model": "'"$MODEL_NAME"'",
  "messages": [{"role": "user", "content": "Say hello in exactly 5 words."}],
  "max_tokens": 50,
  "stream": false
}'

test_endpoint "Chat Completion" "POST" "/v1/chat/completions" "$CHAT_REQUEST"

# Streaming chat
if [ "$STREAMING_TEST" = true ]; then
    STREAM_REQUEST='{
      "model": "'"$MODEL_NAME"'",
      "messages": [{"role": "user", "content": "Count from 1 to 5."}],
      "max_tokens": 50,
      "stream": true
    }'

    test_streaming "Chat Streaming" "/v1/chat/completions" "$STREAM_REQUEST"
fi

# ==============================================================================
# Completions API
# ==============================================================================

echo ""
echo -e "${CYAN}Completions API${NC}"
echo "─────────────────────────────────────────────────────────────────────"

COMPLETION_REQUEST='{
  "model": "'"$MODEL_NAME"'",
  "prompt": "The capital of France is",
  "max_tokens": 20,
  "stream": false
}'

test_endpoint "Text Completion" "POST" "/v1/completions" "$COMPLETION_REQUEST"

# ==============================================================================
# Error Cases
# ==============================================================================

echo ""
echo -e "${CYAN}Error Handling${NC}"
echo "─────────────────────────────────────────────────────────────────────"

test_endpoint "Invalid Model" "POST" "/v1/chat/completions" \
    '{"model": "nonexistent-model", "messages": [{"role": "user", "content": "test"}]}' \
    "404"

test_endpoint "404 Route" "GET" "/v1/nonexistent" "404"

# ==============================================================================
# Summary
# ==============================================================================

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "Results: ${GREEN}$PASSED passed${NC}, ${RED}$FAILED failed${NC}"
echo ""

if [ $FAILED -gt 0 ]; then
    echo -e "${YELLOW}Some tests failed. Check the output above for details.${NC}"
    exit 1
else
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
fi
