#!/bin/bash

# check the required parameters
if [ -z "$1" ] || [ -z "$2" ]; then
    echo "Error: Missing required parameters."
    echo "Usage: $0 <version> <region>"
    echo "Example: $0 v0.1.0 aibrix-container-registry-cn-beijing.cr.volces.com"
    exit 1
fi

# aibrix tag，e.g. v0.1.0
# registry，e.g. aibrix-container-registry-cn-beijing.cr.volces.com
VERSION=$1
REGISTRY=$2

# image list
IMAGES=("runtime" "users" "plugins" "controller-manager")

# pull、retag and push images
for IMAGE in "${IMAGES[@]}"; do
    docker pull aibrix/${IMAGE}:${VERSION}
    docker tag aibrix/${IMAGE}:${VERSION} ${REGISTRY}/aibrix/${IMAGE}:${VERSION}
    docker push ${REGISTRY}/aibrix/${IMAGE}:${VERSION}
done