#!/usr/bin/env bash

set -euo pipefail

EXTERNAL_METRICS_E2E_CLUSTER="${EXTERNAL_METRICS_E2E_CLUSTER:-minikube}"
EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER="${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER:-false}"
EXTERNAL_METRICS_E2E_KEEP_ON_FAILURE="${EXTERNAL_METRICS_E2E_KEEP_ON_FAILURE:-false}"
MINIKUBE_PROFILE="${MINIKUBE_PROFILE:-minikube}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-installation-test}"
IMAGE_TAG="${IMAGE_TAG:-external-metrics-e2e}"
CONTROLLER_IMAGE="aibrix/controller-manager:${IMAGE_TAG}"
ADAPTER_IMAGE="aibrix/external-metrics-adapter:e2e"

cleanup() {
  kubectl delete -k test/e2e/testdata/external-metrics-adapter --ignore-not-found=true || true
  if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" != "true" ]; then
    kubectl delete -k test/e2e/testdata/external-metrics-autoscaler/aibrix --ignore-not-found=true || true
  fi
}

cleanup_on_exit() {
  local status=$?
  if [ "${status}" -ne 0 ] && [ "${EXTERNAL_METRICS_E2E_KEEP_ON_FAILURE}" = "true" ]; then
    echo "preserving external metrics e2e resources for failure diagnostics" >&2
    return
  fi
  cleanup
}

diagnose() {
  echo "=== external metrics e2e diagnostics ===" >&2
  kubectl get apiservice/v1beta1.external.metrics.k8s.io -o yaml || true
  kubectl -n aibrix-system get pods,deploy,svc,endpoints || true
  kubectl -n aibrix-system logs deploy/aibrix-controller-manager --tail=200 || true
  kubectl -n aibrix-system logs deploy/aibrix-autoscaling-controller-manager --tail=200 || true
  kubectl -n aibrix-system logs deploy/aibrix-external-metrics-adapter --tail=200 || true
  kubectl -n default get podautoscaler,deploy,pods,events --sort-by=.lastTimestamp || true
  kubectl -n default get podautoscaler external-metrics-e2e -o yaml || true
  kubectl -n default get deploy external-metrics-scale-target -o yaml || true
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 1
  fi
}

require_cmd docker
require_cmd go
require_cmd kubectl

case "${EXTERNAL_METRICS_E2E_CLUSTER}" in
  minikube)
    require_cmd minikube

    if ! minikube status -p "${MINIKUBE_PROFILE}" >/dev/null 2>&1; then
      minikube start -p "${MINIKUBE_PROFILE}"
    fi

    kubectl config use-context "${MINIKUBE_PROFILE}"
    ;;
  kind)
    require_cmd kind

    if ! kind get clusters | grep -qx "${KIND_CLUSTER_NAME}"; then
      echo "kind cluster not found: ${KIND_CLUSTER_NAME}" >&2
      exit 1
    fi
    ;;
  *)
    echo "unsupported EXTERNAL_METRICS_E2E_CLUSTER=${EXTERNAL_METRICS_E2E_CLUSTER}; expected kind or minikube" >&2
    exit 1
    ;;
esac

trap cleanup_on_exit EXIT

TARGETARCH="${TARGETARCH:-$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')}"
mkdir -p bin
if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" != "true" ]; then
  CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" go build -tags="nozmq" -o bin/controller-manager-e2e cmd/controllers/main.go
fi
CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" go build -o bin/external-metrics-adapter-e2e ./test/e2e/external-metrics-adapter
if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" != "true" ]; then
  docker build -t "${CONTROLLER_IMAGE}" -f test/e2e/controller-manager/Dockerfile .
fi
docker build -t "${ADAPTER_IMAGE}" -f test/e2e/external-metrics-adapter/Dockerfile.local .

case "${EXTERNAL_METRICS_E2E_CLUSTER}" in
  minikube)
    if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" != "true" ]; then
      minikube image load -p "${MINIKUBE_PROFILE}" "${CONTROLLER_IMAGE}"
    fi
    minikube image load -p "${MINIKUBE_PROFILE}" "${ADAPTER_IMAGE}"
    ;;
  kind)
    if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" = "true" ]; then
      kind load docker-image "${ADAPTER_IMAGE}" --name "${KIND_CLUSTER_NAME}"
    else
      kind load docker-image "${CONTROLLER_IMAGE}" "${ADAPTER_IMAGE}" --name "${KIND_CLUSTER_NAME}"
    fi
    ;;
esac

kubectl create namespace aibrix-system --dry-run=client -o yaml | kubectl apply -f -
if [ "${EXTERNAL_METRICS_E2E_USE_EXISTING_CONTROLLER}" = "true" ]; then
  kubectl -n aibrix-system rollout status deployment/aibrix-controller-manager --timeout=180s
else
  kubectl apply -k test/e2e/testdata/external-metrics-autoscaler/aibrix
  kubectl -n aibrix-system rollout status deployment/aibrix-autoscaling-controller-manager --timeout=180s
fi

kubectl apply -k test/e2e/testdata/external-metrics-adapter
kubectl -n aibrix-system rollout status deployment/aibrix-external-metrics-adapter --timeout=180s
kubectl wait --for=condition=Available apiservice/v1beta1.external.metrics.k8s.io --timeout=180s

if ! AIBRIX_EXTERNAL_METRICS_E2E=true AIBRIX_EXTERNAL_METRICS_E2E_KEEP_ON_FAILURE=true \
  go test ./test/e2e -v -run TestExternalMetricsAutoscaler -count=1 -timeout=5m; then
  diagnose
  exit 1
fi
