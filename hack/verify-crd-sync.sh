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

# Verify that CRD files (only) in module directories and the Helm chart are
# synchronized with bases/. Ignores kustomization.yaml and other non-CRD files.
#
# CI runs `make manifests` before this script so config/crd/bases/ exists.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..

# Directories
BASES_DIR="${SCRIPT_ROOT}/config/crd/bases"
ORCHESTRATION_DIR="${SCRIPT_ROOT}/config/crd/orchestration"
AUTOSCALING_DIR="${SCRIPT_ROOT}/config/crd/autoscaling"
MODEL_DIR="${SCRIPT_ROOT}/config/crd/model"
HELM_CRDS_DIR="${SCRIPT_ROOT}/dist/chart/crds"

# Temporary directory for diff
TMP_DIFFROOT="$(mktemp -d -t verify-crd-sync.XXXXXX)"
cleanup() {
  rm -rf "${TMP_DIFFROOT}"
}
trap cleanup EXIT

echo "Verifying CRD synchronization between 'bases' and module directories..."

# Function to compare only CRD files for a single API module.
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

# Verify every CRD in bases/ is present and byte-identical under dist/chart/crds/.
# Also flag any extra chart CRDs that are not generated into bases/.
verify_helm_crd_sync() {
  local src_dir=$1
  local dst_dir=$2
  local all_ok=true

  if [[ ! -d "${src_dir}" ]]; then
    echo "ERROR: Source directory does not exist: ${src_dir}" >&2
    exit 1
  fi

  if [[ ! -d "${dst_dir}" ]]; then
    echo "❌ Helm CRD directory does not exist: ${dst_dir}" >&2
    echo "   Please run 'make sync-crds-to-helm' to populate it." >&2
    return 1
  fi

  echo "Verifying CRD synchronization between 'bases' and Helm chart (dist/chart/crds/)..."

  while IFS= read -r src_file; do
    local filename
    filename=$(basename "$src_file")
    local dst_file="${dst_dir}/${filename}"

    if [[ ! -f "$dst_file" ]]; then
      echo "❌ CRD file missing in Helm chart: ${dst_file}" >&2
      echo "   Please run 'make sync-crds-to-helm' to update it." >&2
      all_ok=false
      continue
    fi

    if ! diff -Naupr "$src_file" "$dst_file" >/dev/null 2>&1; then
      echo "❌ CRD file '${filename}' in '${dst_dir}' differs from 'bases/'." >&2
      echo "   Please run 'make sync-crds-to-helm' to update it." >&2
      all_ok=false
    fi
  done < <(find "${src_dir}" -maxdepth 1 -name "*.yaml" -type f | sort)

  while IFS= read -r dst_file; do
    local filename
    filename=$(basename "$dst_file")
    local src_file="${src_dir}/${filename}"

    if [[ ! -f "$src_file" ]]; then
      echo "❌ Extra CRD in Helm chart not present in bases/: ${dst_file}" >&2
      echo "   Remove it or regenerate bases with 'make manifests'." >&2
      all_ok=false
    fi
  done < <(find "${dst_dir}" -maxdepth 1 -name "*.yaml" -type f | sort)

  if [[ "${all_ok}" == "true" ]]; then
    echo "✅ Helm chart CRDs are synchronized with bases/."
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

if ! verify_helm_crd_sync "${BASES_DIR}" "${HELM_CRDS_DIR}"; then
  all_ok=false
fi

if [[ "${all_ok}" == "true" ]]; then
  echo "🎉 All CRD modules and the Helm chart are synchronized with bases/."
  exit 0
else
  echo "❌ CRD synchronization verification failed."
  exit 1
fi