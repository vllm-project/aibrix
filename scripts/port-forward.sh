#!/bin/bash

# Port forwarding script for AIBrix services

set -e

KUBECTL=${KUBECTL:-kubectl}

check_port_in_use() {
    port="$1"
    if lsof -i :"$port" >/dev/null 2>&1; then
        return 0
    else
        return 1
    fi
}

port_forward() {
    service_name="$1"
    namespace="$2"
    service="$3"
    local_port="$4"
    remote_port="$5"
    pid_file="$6"
    
    if check_port_in_use "$local_port"; then
        echo "  ⚠️  Port $local_port is already in use for $service_name"
        echo "     Checking if it's from another port-forward session..."
        existing_process=$(lsof -i :"$local_port" -t 2>/dev/null | head -1)
        if [ -n "$existing_process" ]; then
            process_info=$(ps -p "$existing_process" -o comm= 2>/dev/null || echo "unknown")
            echo "     Process using port $local_port: PID $existing_process ($process_info)"
            if echo "$process_info" | grep -q kubectl; then
                echo "     ✅ Port $local_port appears to be used by kubectl port-forward"
                echo "     Skipping $service_name (already forwarded)"
                return 0
            else
                echo "     ❌ Port $local_port is used by a different process"
                echo "     Trying alternative port..."
                alt_port=$((local_port + 1000))
                echo "     Attempting to use port $alt_port instead"
                local_port=$alt_port
            fi
        fi
    fi
    
    success=false
    for i in 1 2 3; do
        echo "  - Attempt $i: Starting port-forward for $service_name..."
        error_log=$(mktemp)
        
        $KUBECTL -n "$namespace" port-forward "$service" "$local_port:$remote_port" 2>"$error_log" &
        kubectl_pid=$!
        echo $kubectl_pid > "$pid_file"
        sleep 2
        if kill -0 $kubectl_pid 2>/dev/null; then
            success=true
            rm -f "$error_log"
            break
        else
            echo "    ❌ Port-forward process died immediately"
            if [ -s "$error_log" ]; then
                echo "    Error details: $(cat "$error_log")"
            fi
            rm -f "$error_log"
        fi
        echo "    Warning: Failed to port-forward $service_name. Retrying in 10 seconds..."
        sleep 10
    done
    
    if [ "$success" = false ]; then
        echo "Error: Could not set up port-forwarding for $service_name after 5 attempts"
        make dev-stop-port-forward
        exit 1
    fi
}

echo "Setting up port forwarding for AIBrix services..."

# Start port forwarding for all services
port_forward "Envoy Gateway" "envoy-gateway-system" "service/envoy-aibrix-system-aibrix-eg-903790dc" "8888" "80" ".envoy-pf.pid"
port_forward "Redis" "aibrix-system" "service/aibrix-redis-master" "6379" "6379" ".redis-pf.pid"
port_forward "Prometheus" "prometheus" "service/prometheus-kube-prometheus-prometheus" "9090" "9090" ".prometheus-pf.pid"
port_forward "Grafana" "prometheus" "service/prometheus-grafana" "3000" "80" ".grafana-pf.pid"

echo "Port forwarding setup complete!"
echo "Access services at:"
echo "  - Envoy Gateway: http://localhost:8888 (or alternative port if 8888 was in use)"
echo "  - Redis: localhost:6379"
echo "  - Prometheus: http://localhost:9090"
echo "  - Grafana: http://localhost:3000"
echo ""
echo "To stop port forwarding, run: make dev-stop-port-forward"
echo ""
echo "Press Ctrl+C to stop all port forwarding sessions"

# Keep the shell session alive
tail -f /dev/null 