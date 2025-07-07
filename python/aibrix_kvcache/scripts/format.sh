#!/usr/bin/env bash
set -eo pipefail

# this stops git rev-parse from failing if we run this from the .git directory
SUBDIR="python/aibrix_kvcache/"
ROOT="$(git rev-parse --show-toplevel)/$SUBDIR"
builtin cd "$ROOT" || exit 1

check_command() {
    if ! command -v "$1" &> /dev/null; then
        echo "$1 is not installed, please run \`poetry install --no-root --with dev\`"
        exit 1
    fi
}

check_command pre-commit

export SKIP="suggestion"
pre-commit run --all-files
