#!/bin/bash


# Step 1: Create a namespace to host components
echo "Creating namespace for aibrix..."
kubectl create namespace aibrix-system
if [ $? -ne 0 ]; then
    echo "Failed to create namespace for aibrix."
    exit 1
fi

# Step 2: Install the NVIDIA GPU Operator
echo "Adding NVIDIA Helm repository and Installing NVIDIA GPU Operator..."
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install aibrix-gpu-operator nvidia/gpu-operator --namespace aibrix-system
if [ $? -ne 0 ]; then
    echo "Failed to add NVIDIA Helm repository or failed to install NVIDIA GPU Operator."
    exit 1
fi

# Step 3: Install KubeRay Stack
echo "Adding KubeRay Operator..."
helm repo add kuberay https://ray-project.github.io/kuberay-helm/
helm repo update
helm install aibrix-kuberay-operator kuberay/kuberay-operator --version 1.1.0 --namespace aibrix-system
if [ $? -ne 0 ]; then
    echo "Failed to add KubeRay or update."
    exit 1
fi

echo "Setup complete. All components have been installed successfully."
