#!/bin/bash
set -euo pipefail

DEVICE=${DEVICE:-"mlx5_2"}
PORT=${PORT:-1}
BASE_PATH="/sys/class/infiniband/${DEVICE}/ports/${PORT}/gid_attrs/types"

echo "Scanning GID types in: $BASE_PATH"
if [ ! -d "$BASE_PATH" ]; then
    echo "❌ Directory not found: $BASE_PATH"
    echo "Check if the RDMA device and port are correct and visible in this container."
    exit 1
fi

for GID_FILE in "$BASE_PATH"/*; do
    IDX=$(basename "$GID_FILE")

    if [ ! -f "$GID_FILE" ]; then
        echo "⚠️  Skipping non-regular file: $GID_FILE"
        continue
    fi

    GID_TYPE=$(cat "$GID_FILE" 2> /tmp/gid_err || true)
    if [[ -z "$GID_TYPE" ]]; then
        ERR=$(</tmp/gid_err)
        if echo "$ERR" | grep -q "Invalid argument"; then
            echo "IDX $IDX: ❌ Invalid argument (likely VF or unsupported GID)"
        else
            echo "IDX $IDX: ❌ Error: $ERR"
        fi
    else
        echo "IDX $IDX: ✅ $GID_TYPE"
    fi
done

