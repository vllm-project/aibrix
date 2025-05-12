#!/bin/bash

# check the required parameters
if [ -z "$1" ]; then
    echo "Error: Missing required parameter."
    echo "Usage: $0 <target-registry>"
    echo "Example: $0 aibrix-container-registry-cn-beijing.cr.volces.com"
    exit 1
fi

TARGET_REGISTRY=$1

# List of images to sync in the format "source_image:tag new_repo_path"
IMAGES=(
    "redis:latest ${TARGET_REGISTRY}/aibrix/redis:latest"
    "envoyproxy/envoy:v1.33.2 ${TARGET_REGISTRY}/aibrix/envoy:v1.33.2"
    "envoyproxy/gateway:v1.2.8 ${TARGET_REGISTRY}/aibrix/gateway:v1.2.8"
    "aibrix/kuberay-operator:v1.2.1-patch ${TARGET_REGISTRY}/aibrix/kuberay-operator:v1.2.1-patch"
    "busybox:stable ${TARGET_REGISTRY}/aibrix/busybox:stable"
)

for ITEM in "${IMAGES[@]}"; do
    SRC_IMAGE=$(echo $ITEM | awk '{print $1}')
    DST_IMAGE=$(echo $ITEM | awk '{print $2}')

    echo "=== Syncing $SRC_IMAGE to $DST_IMAGE ==="
    docker pull "$SRC_IMAGE"
    docker tag "$SRC_IMAGE" "$DST_IMAGE"
    docker push "$DST_IMAGE"
done
