#!/usr/bin/env bash

set -euo pipefail
VA_DIRS_LIST=("$@")

SOURCE_DIR="$(dirname "$(realpath "$0")")"
GIT_ROOT="$(git rev-parse --show-toplevel)"
GIT_REPO_NAME="$(basename "$GIT_ROOT")"

if [ -z "$VA_DIRS_LIST" ]; then
  DIRS=($(find "$SOURCE_DIR" -mindepth 1 -maxdepth 1 -type d))
else
  DIRS=()
  for dir in "${VA_DIRS_LIST[@]}"; do
    DIRS+=("$SOURCE_DIR/$dir")
  done
fi

for dir in "${DIRS[@]}"; do
  if [ -f "$dir/Dockerfile" ]; then
    echo "Building $GIT_REPO_NAME/$(basename "$dir") from $dir"
    "$SOURCE_DIR/build_and_push_multiarch.sh" "$dir/Dockerfile" "registry.zielenski.dev/$GIT_REPO_NAME/$(basename "$dir")"
  fi
done
