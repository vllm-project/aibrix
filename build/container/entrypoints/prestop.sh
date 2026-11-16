#!/busybox sh
# Wait for Envoy endpoint updates, then drain this gateway's ext_proc requests.
# Usage: /busybox sh /prestop.sh [metrics-url [propagation-delay [timeout [poll-interval]]]]
# timeout includes the propagation delay and must leave time for SIGTERM cleanup.
set -eu

busybox=${AIBRIX_PRESTOP_BUSYBOX:-/busybox}
metrics_url=${1:-http://127.0.0.1:8080/metrics}
propagation_delay=${2:-10}
timeout=${3:-300}
poll_interval=${4:-2}

log() {
    # Lifecycle exec output is separate from the container's log stream.
    # A subshell keeps a failed redirection from aborting the drain under set -e.
    if ! (printf 'gateway preStop: %s\n' "$*" >> /proc/1/fd/2) 2>/dev/null; then
        printf 'gateway preStop: %s\n' "$*" >&2
    fi
}

if [ "$#" -gt 4 ] || [ -z "${POD_NAME:-}" ]; then
    log 'expected at most four arguments and a non-empty POD_NAME'
    exit 2
fi
for seconds in "$propagation_delay" "$timeout" "$poll_interval"; do
    case "$seconds" in
        ''|*[!0-9]*|0[0-9]*)
            log 'durations must be integers in seconds without leading zeros'
            exit 2
            ;;
    esac
    if [ "${#seconds}" -gt 5 ] || [ "$seconds" -gt 86400 ]; then
        log 'durations must not exceed 86400 seconds'
        exit 2
    fi
done
if [ "$poll_interval" -eq 0 ] || [ "$timeout" -le "$propagation_delay" ]; then
    log 'poll interval must be positive and timeout must exceed propagation delay'
    exit 2
fi

# /proc/uptime is monotonic, unlike the wall clock, and exists in Kubernetes pods.
remaining_time() {
    read -r uptime unused < /proc/uptime
    remaining=$((deadline - ${uptime%%.*}))
}
read -r uptime unused < /proc/uptime
deadline=$((${uptime%%.*} + timeout))
log "waiting ${propagation_delay}s for endpoint updates; total budget ${timeout}s"
"$busybox" sleep "$propagation_delay"

zero_samples=0
while :; do
    remaining_time
    if [ "$remaining" -le 0 ]; then
        # A normal return allows kubelet to send SIGTERM using the reserved time.
        log 'drain deadline reached; allowing SIGTERM (requests may remain)'
        exit 0
    fi
    fetch_timeout=5
    if [ "$remaining" -lt "$fetch_timeout" ]; then
        fetch_timeout=$remaining
    fi

    # Bound the whole fetch, including a peer that keeps trickling response bytes.
    # Disable proxies because this must query the terminating Pod itself.
    if body=$("$busybox" timeout -s KILL "$fetch_timeout" \
        "$busybox" wget -Y off -q -O - "$metrics_url"); then
        if in_flight=$(printf '%s\n' "$body" | "$busybox" awk -v pod="$POD_NAME" '
            $1 == "gateway_in_flight_requests{gateway_pod=\"" pod "\"}" {
                seen++
                if (NF != 2 || $2 !~ /^[+]?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/) {
                    invalid = 1
                }
                count = $2
                if ($2 + 0 != 0) busy = 1
            }
            END {
                if (seen != 1 || invalid) exit 1
                # Normalize zero, including scientific notation, for the shell.
                print busy ? count : "0"
            }
        '); then
            if [ "$in_flight" = 0 ]; then
                zero_samples=$((zero_samples + 1))
                if [ "$zero_samples" -ge 3 ]; then
                    log 'in-flight is zero for three consecutive scrapes; drain complete'
                    exit 0
                fi
            else
                zero_samples=0
                log "requests still in flight: $in_flight"
            fi
        else
            zero_samples=0
            log 'in-flight sample missing or invalid; retrying'
        fi
    else
        zero_samples=0
        log 'metrics fetch failed or timed out; retrying'
    fi

    remaining_time
    if [ "$remaining" -gt 0 ]; then
        sleep_seconds=$poll_interval
        if [ "$remaining" -lt "$sleep_seconds" ]; then
            sleep_seconds=$remaining
        fi
        "$busybox" sleep "$sleep_seconds"
    fi
done
