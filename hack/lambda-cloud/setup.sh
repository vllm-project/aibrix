#!/bin/bash

set -e # Exit on any error

# Step 1: Install the NVIDIA GPU Operator
echo "Adding NVIDIA Helm repository and Installing NVIDIA GPU Operator..."
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install gpu-operator nvidia/gpu-operator --namespace kube-system
if [ $? -ne 0 ]; then
    echo "Failed to add NVIDIA Helm repository or failed to install NVIDIA GPU Operator."
    exit 1
fi

# Step 2: Install the  Cloud Provider Kind
echo "Installing Cloud Provider Kind..."
KIND_CLOUD_PROVIDER_VERSION="0.5.0"
KIND_CLOUD_PROVIDER_URL="https://github.com/kubernetes-sigs/cloud-provider-kind/releases/download/v${KIND_CLOUD_PROVIDER_VERSION}/cloud-provider-kind_0.5.0_linux_amd64.tar.gz"

# Download and extract
curl -L ${KIND_CLOUD_PROVIDER_URL} -o cloud-provider-kind.tar.gz
tar -xvzf cloud-provider-kind.tar.gz
chmod +x cloud-provider-kind
sudo mv cloud-provider-kind /usr/local/bin/

# Verify installation
if ! command -v cloud-provider-kind &> /dev/null; then
    echo "Failed to install Cloud Provider Kind."
    exit 1
fi

# Step 3: Run cloud-provider-kind in the background and forward logs
echo "Starting cloud-provider-kind in the background..."
LOG_FILE="/tmp/cloud-provider-kind.log"

nohup cloud-provider-kind > ${LOG_FILE} 2>&1 &

# Save the process ID
echo $! > /tmp/cloud-provider-kind.pid
echo "Cloud Provider Kind is running in the background. Logs are being written to ${LOG_FILE}."

echo "Setup complete. All components have been installed successfully."
