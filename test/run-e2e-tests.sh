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
    kind create cluster --image kindest/node:${K8S_VERSION} --config=./hack/kind_config.yaml
  fi
fi

if [ -n "$SET_KUBECONFIG" ]; then
  kind get kubeconfig > /tmp/admin.conf
  export KUBECONFIG=/tmp/admin.conf
fi

# build images
if [ -n "$INSTALL_AIBRIX" ]; then
  make docker-build-all
  kind load docker-image aibrix/controller-manager:nightly
  kind load docker-image aibrix/gateway-plugins:nightly
  kind load docker-image aibrix/metadata-service:nightly
  kind load docker-image aibrix/runtime:nightly

  kubectl create -k config/dependency
  kubectl create -k config/default

  cd development/app
  docker build -t aibrix/vllm-mock:nightly -f Dockerfile .
  kind load docker-image aibrix/vllm-mock:nightly
  kubectl create -k config/mock
  cd ../..

  kubectl port-forward svc/llama2-7b 8000:8000 &
  kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc  8888:80 &

  function cleanup {
    echo "Cleaning up..."
    # clean up env at end
    kubectl delete --ignore-not-found=true -k config/default
    kubectl delete --ignore-not-found=true -k config/dependency
    cd development/app
    kubectl delete -k config/mock
    cd ../..
  }

  trap cleanup EXIT
fi

collect_logs() {
  echo "Collecting pods and logs"
  kubectl get pods -n aibrix-system

  for pod in $(kubectl get pods -n aibrix-system -o name); do
    echo "Logs for ${pod}"
    kubectl logs -n aibrix-system ${pod}
  done
}

trap "collect_logs" ERR

go test ./test/e2e/ -v -timeout 0
