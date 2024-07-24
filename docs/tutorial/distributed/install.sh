#!/bin/bash

# Install all the dependencies to build nvkind.
# We do not necessarily need to build it, we can download from public repo.

echo "Starting the installation process..."

# Install required system packages
sudo apt update && sudo apt install -y jq

# Install Go
echo "Installing Go..."
wget https://go.dev/dl/go1.22.3.linux-amd64.tar.gz
sudo tar -zxvf go1.22.3.linux-amd64.tar.gz -C /usr/local
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
echo "Go installation completed."

# Install kubectl
echo "Installing kubectl..."
KUBECTL_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
curl -LO "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
chmod +x kubectl
mkdir -p ~/.local/bin
mv ./kubectl ~/.local/bin/kubectl
echo "kubectl installation completed."

# Install kind
echo "Installing Kind..."
if [ $(uname -m) = "x86_64" ]; then
    curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.22.0/kind-linux-amd64
    chmod +x kind
    mv ./kind ~/.local/bin/kind
    echo "Kind installation completed."
else
    echo "Unsupported architecture for Kind."
fi

# Install Helm
echo "Installing Helm..."
curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
chmod 700 get_helm.sh
./get_helm.sh
echo "Helm installation completed."

# Install nvkind
curl -L -o ~/nvkind-linux-amd64.tar.gz https://github.com/Jeffwan/kind-with-gpus-examples/releases/download/v0.1.0/nvkind-linux-amd64.tar.gz
tar -xzvf ~/nvkind-linux-amd64.tar.gz -C ~/.local/bin/
mv ~/.local/bin/nvkind-linux-amd64 ~/.local/bin/nvkind

echo "All installations completed successfully."

echo "*********************************************************************"

# Adjusting permissions (use with caution due to security implications)
sudo chmod 666 /var/run/docker.sock

# Add the current user to the docker group
sudo usermod -aG docker $USER

echo "*********************************************************************"

echo "Configure the nvidia container toolkits"

# Add the Nvidia Container Toolkit production repository
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg \
    &&  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
        sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
        sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list

# Configure the Nvidia repository to use experimental packages (uncommenting the experimental line if commented)
sudo sed -i '/experimental/ s/^#//g' /etc/apt/sources.list.d/nvidia-container-toolkit.list

# Disable the Lambda Labs repository by commenting out all lines
sudo sed -i 's/^/#/' /etc/apt/sources.list.d/lambda-repository.list

# Remove older versions of the Nvidia Container Toolkit
sudo apt-get remove -y nvidia-container-toolkit
echo "Lagacy nvidia-container-toolkit has been removed successfully."

# Update package lists and install the latest version of Nvidia Container Toolkit
sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit
echo "Latest nvidia-container-toolkit has been installed successfully."

echo "*********************************************************************"

# Write NVIDIA runtime configuration to Docker daemon settings
echo '{
    "runtimes": {
        "nvidia": {
            "path": "/usr/bin/nvidia-container-runtime",
            "runtimeArgs": []
        }
    }
}' | sudo tee /etc/docker/daemon.json > /dev/null

echo "Configuring NVIDIA Container Toolkit settings..."
# Configure NVIDIA Container Toolkit settings
sudo nvidia-ctk runtime configure --runtime=docker --set-as-default --cdi.enabled
sudo nvidia-ctk config --set accept-nvidia-visible-devices-as-volume-mounts=true --in-place
# Restart Docker to apply changes
echo "Restarting docker daemon..."
sudo systemctl restart docker

echo "All Installation and Configuration are done"
