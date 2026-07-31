#!/bin/bash
# ==============================================================================
# AIBrix Docker Compose - Health Check Script
# ==============================================================================
# Usage:
#   ./scripts/health-check.sh           # Check all services
#   ./scripts/health-check.sh --json    # Output as JSON
#   ./scripts/health-check.sh --watch   # Continuous monitoring
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

# Default ports
HTTP_PORT=${HTTP_PORT:-80}
VLLM_PORT=${VLLM_PORT:-8000}
METADATA_PORT=${METADATA_PORT:-8090}
ENVOY_ADMIN_PORT=${ENVOY_ADMIN_PORT:-9901}
REDIS_PORT=${REDIS_PORT:-6379}

# Parse arguments
JSON_OUTPUT=false
WATCH_MODE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --json|-j)
            JSON_OUTPUT=true
            shift
            ;;
        --watch|-w)
            WATCH_MODE=true
            shift
            ;;
        -h|--help)
            echo "AIBrix Health Check Script"
            echo ""
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --json, -j     Output as JSON"
            echo "  --watch, -w    Continuous monitoring (5s interval)"
            echo "  -h, --help     Show this help"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Check service health
check_http() {
    local name=$1
    local url=$2
    local timeout=${3:-5}

    if curl -sf --max-time "$timeout" "$url" > /dev/null 2>&1; then
        echo "healthy"
    else
        echo "unhealthy"
    fi
}

check_redis() {
    if docker compose exec -T redis redis-cli ping 2>/dev/null | grep -q "PONG"; then
        echo "healthy"
    else
        echo "unhealthy"
    fi
}

get_container_status() {
    local service=$1
    docker compose ps --format json "$service" 2>/dev/null | jq -r '.Status // "not running"' 2>/dev/null || echo "not running"
}

run_health_check() {
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

    # Check each service
    local redis_status=$(check_redis)
    local envoy_status=$(check_http "envoy" "http://localhost:${ENVOY_ADMIN_PORT}/ready")
    local metadata_status=$(check_http "metadata" "http://localhost:${METADATA_PORT}/healthz")
    local vllm_status=$(check_http "vllm" "http://localhost:${VLLM_PORT}/health" 10)
    local proxy_status=$(check_http "proxy" "http://localhost:${HTTP_PORT}/healthz")

    if [ "$JSON_OUTPUT" = true ]; then
        # JSON output
        cat <<EOF
{
  "timestamp": "$timestamp",
  "services": {
    "redis": {"status": "$redis_status", "port": $REDIS_PORT},
    "envoy": {"status": "$envoy_status", "port": $ENVOY_ADMIN_PORT},
    "metadata-service": {"status": "$metadata_status", "port": $METADATA_PORT},
    "vllm": {"status": "$vllm_status", "port": $VLLM_PORT},
    "proxy": {"status": "$proxy_status", "port": $HTTP_PORT}
  },
  "overall": "$([ "$redis_status" = "healthy" ] && [ "$envoy_status" = "healthy" ] && [ "$vllm_status" = "healthy" ] && echo "healthy" || echo "degraded")"
}
EOF
    else
        # Human-readable output
        echo ""
        echo -e "${CYAN}AIBrix Health Check${NC} - $timestamp"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo ""

        print_status() {
            local name=$1
            local status=$2
            local port=$3

            if [ "$status" = "healthy" ]; then
                printf "  %-20s ${GREEN}●${NC} healthy     :%s\n" "$name" "$port"
            else
                printf "  %-20s ${RED}●${NC} unhealthy   :%s\n" "$name" "$port"
            fi
        }

        print_status "Redis" "$redis_status" "$REDIS_PORT"
        print_status "Envoy" "$envoy_status" "$ENVOY_ADMIN_PORT"
        print_status "Metadata Service" "$metadata_status" "$METADATA_PORT"
        print_status "vLLM" "$vllm_status" "$VLLM_PORT"
        print_status "HTTP Proxy" "$proxy_status" "$HTTP_PORT"

        echo ""

        # Overall status
        if [ "$redis_status" = "healthy" ] && [ "$envoy_status" = "healthy" ] && [ "$vllm_status" = "healthy" ]; then
            echo -e "  Overall: ${GREEN}All services healthy${NC}"
        else
            echo -e "  Overall: ${YELLOW}Some services degraded${NC}"
        fi

        echo ""
    fi
}

# Main
if [ "$WATCH_MODE" = true ]; then
    echo "Watching health status (Ctrl+C to stop)..."
    while true; do
        clear
        run_health_check
        sleep 5
    done
else
    run_health_check
fi
