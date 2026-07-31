#!/usr/bin/env bash

# Copyright 2025 Aibrix Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Verify that CRD files (only) in module directories are synchronized with bases/.
# Ignores kustomization.yaml and other non-CRD files.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..

# Directories
BASES_DIR="${SCRIPT_ROOT}/config/crd/bases"
ORCHESTRATION_DIR="${SCRIPT_ROOT}/config/crd/orchestration"
AUTOSCALING_DIR="${SCRIPT_ROOT}/config/crd/autoscaling"
MODEL_DIR="${SCRIPT_ROOT}/config/crd/model"

# Temporary directory for diff
TMP_DIFFROOT="$(mktemp -d -t verify-crd-sync.XXXXXX)"
cleanup() {
  rm -rf "${TMP_DIFFROOT}"
}
trap cleanup EXIT

echo "Verifying CRD synchronization between 'bases' and module directories..."

# Function to compare only CRD files
verify_crd_sync() {
  local src_dir=$1
  local dst_dir=$2
  local module=$3

  if [[ ! -d "${src_dir}" ]]; then
    echo "ERROR: Source directory does not exist: ${src_dir}" >&2
    exit 1
  fi

  if [[ ! -d "${dst_dir}" ]]; then
    echo "ERROR: Module directory does not exist: ${dst_dir}" >&2
    exit 1
  fi

  # Regex pattern to match CRD files for the current module
  # Example: orchestration.aibrix.ai_stormservices.yaml
  local pattern="^${module}\\.aibrix\\.ai_.*\\.yaml$"
  local all_ok=true

  # Iterate over all .yaml files in the source directory
  while IFS= read -r src_file; do
    local filename=$(basename "$src_file")
    if [[ ! "$filename" =~ $pattern ]]; then
      continue
    fi

    local dst_file="${dst_dir}/${filename}"

    if [[ ! -f "$dst_file" ]]; then
      echo "❌ CRD file missing in module directory: ${dst_file}" >&2
      all_ok=false
      continue
    fi

    # Compare the content of the source and destination files
    if ! diff -Naupr "$src_file" "$dst_file" >/dev/null 2>&1; then
      echo "❌ CRD file '${filename}' in '${dst_dir}' differs from 'bases/'." >&2
      echo "   Please run 'make sync-crds' to update it." >&2
      all_ok=false
    fi
  done < <(find "${src_dir}" -maxdepth 1 -name "*.yaml" -type f | sort)

  if [[ "${all_ok}" == "true" ]]; then
    echo "✅ ${module} CRDs are synchronized."
    return 0
  else
    return 1
  fi
}
# Run verification for each module
all_ok=true

declare -A modules=(
  ["orchestration"]="${ORCHESTRATION_DIR}"
  ["autoscaling"]="${AUTOSCALING_DIR}"
  ["model"]="${MODEL_DIR}"
)

for module in "${!modules[@]}"; do
  if ! verify_crd_sync "${BASES_DIR}" "${modules[$module]}" "${module}"; then
    all_ok=false
  fi
done

if [[ "${all_ok}" == "true" ]]; then
  echo "🎉 All CRD modules are synchronized with bases/."
  exit 0
else
  echo "❌ CRD synchronization verification failed."
  exit 1
fi