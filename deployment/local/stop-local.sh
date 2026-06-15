#!/usr/bin/env bash
# Stop AIBrix gateway local processes started by run-local.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PID_FILE="${SCRIPT_DIR}/.pids"

if [ ! -f "${PID_FILE}" ]; then
    echo "No running processes found (${PID_FILE} does not exist)."
    exit 0
fi

read -r GATEWAY_PID ENVOY_PID < "${PID_FILE}"

echo "Stopping AIBrix gateway..."

for name_pid in "gateway-plugin:${GATEWAY_PID}" "envoy:${ENVOY_PID}"; do
    name="${name_pid%%:*}"
    pid="${name_pid##*:}"
    if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}"
        echo "  Stopped ${name} (PID ${pid})"
    else
        echo "  ${name} (PID ${pid}) already stopped"
    fi
done

rm -f "${PID_FILE}"

# Clean up Envoy shared memory to prevent stale base_id conflicts on restart
rm -f /dev/shm/envoy_shared_memory_* 2>/dev/null

echo "Done."
