#!/bin/bash

# This shell is used to auto generate some useful tools for k8s, such as clientset, lister, informer and so on.
# We don't use this tool to generate deepcopy because kubebuilder (controller-tools) has coverred that part.

set -o errexit
set -o nounset
set -o pipefail

cd "$(dirname "${0}")/.."

CODEGEN_VERSION=$(grep 'k8s.io/code-generator' go.sum | awk '{print $2}' | sed 's/\/go.mod//g' | head -1)
CODEGEN_PKG=$(echo `go env GOPATH`"/pkg/mod/k8s.io/code-generator@${CODEGEN_VERSION}")
if [[ ! -d ${CODEGEN_PKG} ]]; then
    echo "${CODEGEN_PKG} is missing. Running 'go mod download'."
    go mod download
fi
echo ">> Using ${CODEGEN_PKG}"

REPO_ROOT="$(git rev-parse --show-toplevel)"

source "${CODEGEN_PKG}/kube_codegen.sh"

# TODO: remove the workaround when the issue is solved in the code-generator
# (https://github.com/kubernetes/code-generator/issues/165).
mkdir -p github.com && ln -s ../.. github.com/vllm-project
trap "rm -r github.com" EXIT

kube::codegen::gen_helpers github.com/vllm-project/aibrix/api \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"

kube::codegen::gen_client github.com/vllm-project/aibrix/api \
    --with-watch \
    --with-applyconfig \
    --output-dir "$REPO_ROOT"/pkg/client \
    --output-pkg github.com/vllm-project/aibrix/pkg/client \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"
