#!/bin/bash

# Run nvidia-smi to list GPU devices
nvidia-smi -L
if [ $? -ne 0 ]; then
    echo "nvidia-smi failed to execute."
    exit 1
fi

# Run a Docker container with NVIDIA runtime to list GPU devices
docker run --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=all ubuntu:20.04 nvidia-smi -L
if [ $? -ne 0 ]; then
    echo "Docker command with NVIDIA runtime failed to execute."
    exit 1
fi

# Run a Docker container with mounted /dev/null to check GPU accessibility
docker run -v /dev/null:/var/run/nvidia-container-devices/all ubuntu:20.04 nvidia-smi -L
if [ $? -ne 0 ]; then
    echo "Docker command with mounted /dev/null failed to execute."
    exit 1
fi

echo "All verification checks passed successfully."
