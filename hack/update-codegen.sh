#!/bin/bash

# Generate Kubernetes clients and helpers with the version selected by go.mod.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${REPO_ROOT}"

go mod download k8s.io/code-generator
CODEGEN_PKG="$(go list -m -f '{{.Dir}}' k8s.io/code-generator)"
echo ">> Using ${CODEGEN_PKG}"

source "${CODEGEN_PKG}/kube_codegen.sh"

# Absolute input paths also work when a checkout or worktree is not named aibrix.
kube::codegen::gen_helpers "${REPO_ROOT}/api" \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"

kube::codegen::gen_client "${REPO_ROOT}/api" \
    --with-watch \
    --with-applyconfig \
    --output-dir "$REPO_ROOT"/pkg/client \
    --output-pkg github.com/vllm-project/aibrix/pkg/client \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"
