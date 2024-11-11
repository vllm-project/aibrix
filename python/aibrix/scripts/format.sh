#!/usr/bin/env bash
# ruff linter and formatter, adapted from vLLM.
#
# Usage:
#    # Do work and commit your work.

#    # Format files that differ from origin/main.
#    bash ./scripts/format.sh

# Cause the script to exit if a single command fails
set -eo pipefail

# this stops git rev-parse from failing if we run this from the .git directory
ROOT="$(git rev-parse --show-toplevel)/python/aibrix"
builtin cd "$ROOT" || exit 1

check_command() {
    if ! command -v "$1" &> /dev/null; then
        echo "$1 is not installed, please run \`poetry install --no-root --with dev\`"
        exit 1
    fi
}

check_command ruff
check_command mypy

# check versions
RUFF_VERSION=$(ruff --version | awk '{print $2}')
MYPY_VERSION=$(mypy --version | awk '{print $2}')

# # params: tool name
tool_version_from_pyproject() {
    echo $(grep "$1 =" pyproject.toml | cut -d'=' -f2 | sed 's/^[" \t]*//;s/[" \t]*$//')
}

# # params: tool name, tool version, required version
tool_version_check() {
    if [[ "$2" != "$3" ]]; then
        echo "❓❓Wrong $1 version installed: $3 is required, not $2."
        exit 1
    fi
}

tool_version_check "ruff" "$RUFF_VERSION" $(tool_version_from_pyproject "ruff")
tool_version_check "mypy" "$MYPY_VERSION" $(tool_version_from_pyproject "mypy")

# Run Ruff
echo 'ruff (lint):'
python -m ruff check . --fix
echo 'ruff (format):'
python -m ruff format .
echo 'ruff: Done'

# Run mypy
echo 'mypy:'
python -m mypy .
echo 'mypy: Done'

if ! git diff --quiet &>/dev/null; then
    echo 
    echo "🔍🔍There are files changed by the format checker or by you that are not added and committed:"
    git --no-pager diff --name-only
    echo "🔍🔍Format checker passed, but please add, commit and push all the files above to include changes made by the format checker."

    exit 1
else
    echo "✨🎉 Format check passed! Congratulations! 🎉✨"
fi