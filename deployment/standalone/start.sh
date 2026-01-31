#!/bin/bash
# ==============================================================================
# AIBrix Docker Compose - Start Script
# ==============================================================================
# Usage:
#   ./start.sh              # Start with gateway-plugin (default)
#   ./start.sh --pd         # Start in P/D disaggregation mode
#   ./start.sh --no-pull    # Start without pulling latest images
#   ./start.sh --help       # Show help
# ==============================================================================

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m' # No Color

# Default options
PD_MODE=false
PULL_IMAGES=true
SKIP_CONFIRM=false
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --pd|--disaggregate)
            PD_MODE=true
            shift
            ;;
        --no-pull)
            PULL_IMAGES=false
            shift
            ;;
        -y|--yes)
            SKIP_CONFIRM=true
            shift
            ;;
        -h|--help)
            echo "AIBrix Docker Compose Start Script"
            echo ""
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --pd, --disaggregate   Start in P/D disaggregation mode (2 GPUs)"
            echo "  --no-pull              Skip pulling latest Docker images"
            echo "  -y, --yes              Skip confirmation prompts"
            echo "  -h, --help             Show this help message"
            echo ""
            echo "Architecture:"
            echo "  Envoy + Gateway-plugin (ext_proc) + vLLM engine(s)"
            echo ""
            echo "  The gateway-plugin provides intelligent routing, rate limiting,"
            echo "  and request tracking via Envoy's External Processor filter."
            echo ""
            echo "Modes:"
            echo "  Default                Single vLLM engine with gateway-plugin"
            echo "  P/D                    Separate prefill/decode engines (requires 2 GPUs)"
            echo ""
            echo "Examples:"
            echo "  $0                     # Default mode with gateway-plugin"
            echo "  $0 --pd                # P/D mode with prefill + decode engines"
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

cd "$SCRIPT_DIR"

# ==============================================================================
# Header
# ==============================================================================

echo ""
echo -e "${BOLD}${BLUE}╔══════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${BLUE}║                    AIBrix Docker Compose                         ║${NC}"
echo -e "${BOLD}${BLUE}╚══════════════════════════════════════════════════════════════════╝${NC}"
echo ""

MODE_DESC="Gateway + vLLM (Intelligent routing)"
if [ "$PD_MODE" = true ]; then
    MODE_DESC="Gateway + P/D Disaggregation (Prefill + Decode engines)"
fi
echo -e "${CYAN}Mode:${NC} $MODE_DESC"
echo ""

# ==============================================================================
# Check .env file
# ==============================================================================

if [ ! -f .env ]; then
    echo -e "${YELLOW}No .env file found. Creating from template...${NC}"
    cp .env.example .env
    echo -e "${GREEN}Created .env from .env.example${NC}"
    echo ""
    echo -e "${YELLOW}Please review and edit .env with your configuration:${NC}"
    echo "  - MODEL_NAME: The HuggingFace model ID"
    echo "  - MODEL_DIR:  Path to your HuggingFace cache"
    echo "  - HF_TOKEN:   Your HuggingFace token (for gated models)"
    echo "  - VLLM_GPU:   GPU ID for vLLM engine"
    echo ""
    if [ "$SKIP_CONFIRM" = false ]; then
        read -p "Edit .env now? (Y/n) " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Nn]$ ]]; then
            ${EDITOR:-vi} .env
        fi
    fi
fi

# Source environment variables
set -a
source .env
set +a

# ==============================================================================
# Prerequisites Check
# ==============================================================================

echo -e "${BOLD}Checking prerequisites...${NC}"
echo ""

check_command() {
    if command -v "$1" &> /dev/null; then
        echo -e "  ${GREEN}✓${NC} $2"
        return 0
    else
        echo -e "  ${RED}✗${NC} $2 - $3"
        return 1
    fi
}

# Check Docker
check_command docker "Docker" "Install from https://docs.docker.com/get-docker/" || exit 1

# Check Docker Compose
if docker compose version &> /dev/null; then
    echo -e "  ${GREEN}✓${NC} Docker Compose"
else
    echo -e "  ${RED}✗${NC} Docker Compose - Install Docker Compose v2"
    exit 1
fi

# Check NVIDIA runtime (optional check - don't exit if it fails)
if docker info 2>/dev/null | grep -q "nvidia"; then
    echo -e "  ${GREEN}✓${NC} NVIDIA Container Runtime"
else
    echo -e "  ${YELLOW}!${NC} NVIDIA Container Runtime - GPU support may not work"
    echo -e "      Install: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html"
fi

# Check model directory
MODEL_DIR_EXPANDED="${MODEL_DIR/#\~/$HOME}"
if [ -d "$MODEL_DIR_EXPANDED" ]; then
    echo -e "  ${GREEN}✓${NC} Model directory: $MODEL_DIR"
else
    echo -e "  ${YELLOW}!${NC} Model directory not found: $MODEL_DIR"
    echo -e "      Will be created when model downloads"
fi

echo ""

# ==============================================================================
# Configuration Summary
# ==============================================================================

echo -e "${BOLD}Configuration:${NC}"
echo ""
echo -e "  ${CYAN}Model:${NC}        ${MODEL_NAME:-Qwen/Qwen2.5-1.5B-Instruct}"
echo -e "  ${CYAN}Model Dir:${NC}    ${MODEL_DIR:-~/.cache/huggingface}"

if [ "$PD_MODE" = true ]; then
    echo -e "  ${CYAN}Prefill GPU:${NC}  ${PREFILL_GPU:-0}"
    echo -e "  ${CYAN}Decode GPU:${NC}   ${DECODE_GPU:-1}"
else
    echo -e "  ${CYAN}GPU:${NC}          ${VLLM_GPU:-0}"
fi

echo -e "  ${CYAN}HTTP Port:${NC}    ${HTTP_PORT:-80}"
echo -e "  ${CYAN}vLLM Port:${NC}    ${VLLM_PORT:-8000}"
echo ""

# ==============================================================================
# Confirmation
# ==============================================================================

if [ "$SKIP_CONFIRM" = false ]; then
    read -p "Start AIBrix services? (Y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Nn]$ ]]; then
        echo "Cancelled."
        exit 0
    fi
fi

echo ""

# ==============================================================================
# Configure Gateway for the selected mode
# ==============================================================================

echo -e "${BOLD}Configuring gateway-plugin...${NC}"

# Set endpoints config and routing algorithm based on mode
if [ "$PD_MODE" = true ]; then
    # P/D mode: use endpoints-pd.yaml with prefill/decode roles
    export ENDPOINTS_CONFIG="./configs/endpoints-pd.yaml"
    export ROUTING_ALGORITHM="pd"
    echo -e "  ${CYAN}Config:${NC}    endpoints-pd.yaml"
    echo -e "  ${CYAN}Backends:${NC}  prefill-engine (prefill), decode-engine (decode)"
    echo -e "  ${CYAN}Routing:${NC}   P/D disaggregation"
else
    # Default mode: single vLLM backend
    export ENDPOINTS_CONFIG="./configs/endpoints.yaml"
    export ROUTING_ALGORITHM="${ROUTING_ALGORITHM:-least_request}"
    echo -e "  ${CYAN}Config:${NC}    endpoints.yaml"
    echo -e "  ${CYAN}Backends:${NC}  vllm"
    echo -e "  ${CYAN}Routing:${NC}   ${ROUTING_ALGORITHM}"
fi
echo ""

# ==============================================================================
# Build Docker Compose profiles
# ==============================================================================

PROFILES=""
if [ "$PD_MODE" = true ]; then
    PROFILES="$PROFILES --profile pd"
fi

# ==============================================================================
# Pull Images
# ==============================================================================

if [ "$PULL_IMAGES" = true ]; then
    echo -e "${BOLD}Pulling Docker images...${NC}"
    docker compose $PROFILES pull
    echo ""
fi

# ==============================================================================
# Start Services
# ==============================================================================

echo -e "${BOLD}Starting services...${NC}"
echo ""

if [ "$PD_MODE" = true ]; then
    # In P/D mode, stop the default vllm service if running
    docker compose stop vllm 2>/dev/null || true
    docker compose rm -f vllm 2>/dev/null || true
fi

# Start with the appropriate profiles
docker compose $PROFILES up -d

# ==============================================================================
# Wait for Services
# ==============================================================================

echo ""
echo -e "${BOLD}Waiting for services to be ready...${NC}"
echo ""

# Function to check service health
wait_for_service() {
    local service=$1
    local url=$2
    local timeout=$3
    local elapsed=0

    printf "  Waiting for %-20s " "$service..."

    while [ $elapsed -lt $timeout ]; do
        if curl -sf "$url" > /dev/null 2>&1; then
            echo -e "${GREEN}ready${NC}"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done

    echo -e "${YELLOW}timeout${NC}"
    return 1
}

# Wait for Redis
wait_for_service "redis" "http://localhost:${REDIS_PORT:-6379}" 30 2>/dev/null || true

# Wait for metadata service
wait_for_service "metadata-service" "http://localhost:${METADATA_PORT:-8090}/healthz" 60

# Wait for Envoy
wait_for_service "envoy" "http://localhost:${ENVOY_ADMIN_PORT:-9901}/ready" 30

# Wait for vLLM engine(s) - this takes longer
echo ""
echo -e "${CYAN}Waiting for vLLM to load model (this may take several minutes)...${NC}"

if [ "$PD_MODE" = true ]; then
    wait_for_service "prefill-engine" "http://localhost:${PREFILL_PORT:-8001}/health" 600
    wait_for_service "decode-engine" "http://localhost:${DECODE_PORT:-8002}/health" 600
else
    wait_for_service "vllm" "http://localhost:${VLLM_PORT:-8000}/health" 600
fi

# Wait for gateway
echo ""
echo -e "${CYAN}Waiting for gateway to be ready...${NC}"
wait_for_service "gateway" "http://localhost:${GATEWAY_METRICS_PORT:-8080}/metrics" 60

echo ""

# ==============================================================================
# Service Status
# ==============================================================================

echo -e "${BOLD}Service Status:${NC}"
echo ""
docker compose ps --format "table {{.Name}}\t{{.Status}}\t{{.Ports}}"

# ==============================================================================
# Success Message
# ==============================================================================

echo ""
echo -e "${BOLD}${GREEN}╔══════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${GREEN}║                    AIBrix is running!                            ║${NC}"
echo -e "${BOLD}${GREEN}╚══════════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "${BOLD}Endpoints:${NC}"
echo ""
echo -e "  ${CYAN}Chat API:${NC}      http://localhost:${HTTP_PORT:-80}/v1/chat/completions"
echo -e "  ${CYAN}Models API:${NC}    http://localhost:${HTTP_PORT:-80}/v1/models"
echo -e "  ${CYAN}Health:${NC}        http://localhost:${HTTP_PORT:-80}/health"
echo -e "  ${CYAN}Envoy Admin:${NC}   http://localhost:${ENVOY_ADMIN_PORT:-9901}/"
echo -e "  ${CYAN}Gateway Metrics:${NC} http://localhost:${GATEWAY_METRICS_PORT:-8080}/metrics"

if [ "$PD_MODE" = true ]; then
    echo -e "  ${CYAN}Prefill:${NC}       http://localhost:${PREFILL_PORT:-8001}/"
    echo -e "  ${CYAN}Decode:${NC}        http://localhost:${DECODE_PORT:-8002}/"
else
    echo -e "  ${CYAN}Direct vLLM:${NC}   http://localhost:${VLLM_PORT:-8000}/"
fi
echo ""
echo -e "${BOLD}Quick Test:${NC}"
echo ""
echo "  curl http://localhost:${HTTP_PORT:-80}/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{"
echo "      \"model\": \"${MODEL_NAME:-Qwen/Qwen2.5-1.5B-Instruct}\","
echo "      \"messages\": [{\"role\": \"user\", \"content\": \"Hello!\"}]"
echo "    }'"
echo ""
echo -e "${BOLD}Commands:${NC}"
echo ""
echo "  View logs:     docker compose logs -f"
echo "  View vLLM:     docker compose logs -f vllm"
echo "  Status:        docker compose ps"
echo "  Stop:          docker compose down"
echo "  Restart:       docker compose restart"
echo ""
