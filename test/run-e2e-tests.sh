#!/usr/bin/env bash

# Copyright 2024 The Aibrix Team.

# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at

#     http://www.apache.org/licenses/LICENSE-2.0

# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This script runs end-to-end tests for the Aibrix project.
# It sets up the environment, manages port-forwards, and ensures cleanup.

set -x
set -o errexit
set -o nounset

# Set to empty if unbound/empty
KIND_E2E=${KIND_E2E:-}
SKIP_KUBECTL_INSTALL=${SKIP_KUBECTL_INSTALL:-true}
SKIP_KIND_INSTALL=${SKIP_KIND_INSTALL:-true}
SKIP_INSTALL=${SKIP_INSTALL:-}
SET_KUBECONFIG=${SET_KUBECONFIG:-}
INSTALL_AIBRIX=${INSTALL_AIBRIX:-}

# setup kind cluster
if [ -n "$KIND_E2E" ]; then
  K8S_VERSION=${KUBERNETES_VERSION:-v1.32.0}
  if [ -z "${SKIP_KUBECTL_INSTALL}" ]; then
    curl -Lo kubectl https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/amd64/kubectl && chmod +x kubectl && mv kubectl /usr/local/bin/
  fi
  if [ -z "${SKIP_KIND_INSTALL}" ]; then
    wget https://github.com/kubernetes-sigs/kind/releases/download/v0.26.0/kind-linux-amd64
    chmod +x kind-linux-amd64
    mv kind-linux-amd64 kind
    export PATH=$PATH:$PWD
  fi

  # If we did not set SKIP_INSTALL
  if [ -z "$SKIP_INSTALL" ]; then
    kind create cluster --image kindest/node:${K8S_VERSION} --config=./hack/ci/kind-config.yaml
  fi
fi

if [ -n "$SET_KUBECONFIG" ]; then
  kind get kubeconfig > /tmp/admin.conf
  export KUBECONFIG=/tmp/admin.conf
fi

# build images
if [ -n "$INSTALL_AIBRIX" ]; then
  make docker-build-all
  kind load docker-image aibrix/controller-manager:nightly aibrix/gateway-plugins:nightly aibrix/metadata-service:nightly aibrix/runtime:nightly

  kubectl apply -k config/dependency --server-side
  kubectl apply -k config/test

  cd development/app
  docker build -t aibrix/vllm-mock:nightly -f Dockerfile .
  kind load docker-image aibrix/vllm-mock:nightly
  kubectl apply -k config/mock
  cd ../..

  trap cleanup EXIT
fi

# Function to kill existing port-forward processes
kill_existing_port_forwards() {
  # If a PID file exists, kill all processes listed in it
  if [ -f /tmp/aibrix-port-forwards.pid ]; then
    echo "Killing existing port-forward processes..."
    while read pid; do
      if ps -p $pid > /dev/null; then
        kill $pid 2>/dev/null || true
      fi
    done < /tmp/aibrix-port-forwards.pid
    rm -f /tmp/aibrix-port-forwards.pid
  fi
}

# Start port forwarding and store PIDs
start_port_forwards() {
  # Kill any existing port-forwards first
  kill_existing_port_forwards

  # Start new port-forwards and store PIDs
  echo "Starting port-forwarding..."
  # Each port-forward runs in the background and its PID is saved
  kubectl port-forward svc/llama2-7b 8000:8000 >/dev/null 2>&1 & echo $! >> /tmp/aibrix-port-forwards.pid
  kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 >/dev/null 2>&1 & echo $! >> /tmp/aibrix-port-forwards.pid
  kubectl -n aibrix-system port-forward service/aibrix-redis-master 6379:6379 >/dev/null 2>&1 & echo $! >> /tmp/aibrix-port-forwards.pid
}

# Comprehensive cleanup function that handles both k8s resources and port forwards
function cleanup {
  echo "Running cleanup..."
  # Always kill port forwards when exiting
  kill_existing_port_forwards

  if [ -n "$INSTALL_AIBRIX" ]; then
    echo "Cleaning up k8s resources..."
    # Clean up k8s resources if INSTALL_AIBRIX is set
    kubectl delete --ignore-not-found=true -k config/test
    kubectl delete --ignore-not-found=true -k config/dependency
    cd development/app
    kubectl delete -k config/mock
    cd ../..
  else
    echo "Skipping k8s cleanup as INSTALL_AIBRIX is not set"
  fi
}

# Set up single trap for cleanup
trap cleanup EXIT

# Function to collect logs on error
collect_logs() {
  echo "Collecting pods and logs"
  kubectl get pods -n aibrix-system

  for pod in $(kubectl get pods -n aibrix-system -o name); do
    echo "Logs for ${pod}"
    kubectl logs -n aibrix-system ${pod}
  done
}

trap "collect_logs" ERR

# Start port forwarding before running tests
start_port_forwards

# Run tests using gotestsum
# The test exit code is captured and used as the script's exit code
# so CI can detect failures

echo "Running e2e tests..."
go test ./test/e2e/ -v -timeout 0
TEST_EXIT_CODE=$?

# Exit with the test's exit code
exit $TEST_EXIT_CODE
