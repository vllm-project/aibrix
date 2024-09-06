#!/bin/bash

# This shell is used to auto generate some useful tools for k8s, such as clientset, lister, informer and so on.
# We don't use this tool to generate deepcopy because kubebuilder (controller-tools) has coverred that part.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
DIFFROOT="${SCRIPT_ROOT}/pkg/client"
TMP_DIFFROOT="$(mktemp -d -t "$(basename "$0").XXXXXX")"

cleanup() {
  rm -rf "${TMP_DIFFROOT}"
}
trap "cleanup" EXIT SIGINT

cleanup

mkdir -p "${TMP_DIFFROOT}"
cp -a "${DIFFROOT}"/* "${TMP_DIFFROOT}"

# Ensure we can execute.
chmod +x ${SCRIPT_ROOT}/hack/update-codegen.sh

"${SCRIPT_ROOT}/hack/update-codegen.sh"
echo "diffing ${DIFFROOT} against freshly generated codegen"
ret=0
diff -Naupr "${DIFFROOT}" "${TMP_DIFFROOT}" || ret=$?
if [[ $ret -eq 0 ]]; then
  echo "${DIFFROOT} up to date."
else
  echo "${DIFFROOT} is out of date. Please run hack/update-codegen.sh"
  exit 1
fi

# smoke test
echo "Smoke testing by compiling..."
pushd "${SCRIPT_ROOT}"
  go build "${DIFFROOT}/..."
popd