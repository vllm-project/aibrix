#!/bin/sh
# Keep the kvcached runtime agent alive across an unexpected exit. Engines run
# in their own sessions and remain available for the replacement agent to
# re-adopt from the local registry. Do not use `set -e`: wait intentionally
# observes a non-zero agent exit before restarting it.

child_pid=""

shutdown() {
    trap - INT TERM
    if [ -n "$child_pid" ]; then
        kill -TERM "$child_pid" 2>/dev/null || true
        wait "$child_pid" 2>/dev/null || true
    fi
    exit 0
}

trap shutdown INT TERM

while :; do
    aibrix_runtime "$@" &
    child_pid=$!
    wait "$child_pid"
    exit_status=$?
    child_pid=""
    printf '%s\n' "aibrix_runtime exited unexpectedly (status ${exit_status}); restarting" >&2
    sleep 1
done
