#!/usr/bin/env bash

set -euo pipefail

# Configuration
DOCKERFILE_PATH="$1"                                    # Path to the Dockerfile (passed as argument)
IMAGE_NAME="$2"                                         # Name of the image
shift 2
EXTRA_BUILD_ARGS=("$@")                                 # Extra build arguments

DOCKERFILE_DIR="$(dirname "$DOCKERFILE_PATH")"          # Directory of the Dockerfile
PLATFORMS=("linux/amd64" "linux/arm64")                   # Define target architectures. "linux/arm/v7" used to be here but takes way too long and we have no hosts that need it
if [ -f "$DOCKERFILE_DIR/PLATFORMS" ]; then
  PLATFORMS=($(cat "$DOCKERFILE_DIR/PLATFORMS"))
fi

PREBUILD_SCRIPT="$DOCKERFILE_DIR/build.sh"           # Path to the prebuild script

# Check if Dockerfile path is provided
if [[ -z "$DOCKERFILE_PATH" ]]; then
  echo "Usage: $0 <path-to-dockerfile>" >&2
  exit 1
fi

# Ensure Docker Buildx is available
if ! docker buildx version > /dev/null 2>&1; then
  echo "Docker Buildx is not installed or enabled. Install it and try again." >&2
  exit 1
fi

DOCKER_CONTEXT=${DOCKER_CONTEXT:-multiarch-builder}

# Create a Buildx builder if none exists
if ! docker buildx inspect ${DOCKER_CONTEXT} > /dev/null 2>&1; then
  docker buildx create --name ${DOCKER_CONTEXT}
fi

# Build the multi-architecture image
docker buildx use ${DOCKER_CONTEXT}

if [ -f "$PREBUILD_SCRIPT" ]; then
  echo "Running prebuild script..." >&2
  "$PREBUILD_SCRIPT"
fi

echo "Building image for platforms: ${PLATFORMS[*]}" >&2
docker buildx build \
  --platform "$(IFS=,; echo "${PLATFORMS[*]}")" \
  --tag "${IMAGE_NAME}" \
  --file "$DOCKERFILE_PATH" \
  "${EXTRA_BUILD_ARGS[@]}" \
  "$(dirname "$DOCKERFILE_PATH")" \
  --push

# Verify the pushed image
echo "Image pushed successfully to ${IMAGE_NAME}" >&2
echo "Inspecting the multi-arch image..." >&2
docker buildx imagetools inspect "${IMAGE_NAME}" --format "{{json .Manifest}}" | jq -r '.digest' > "$(dirname "$DOCKERFILE_PATH")/LATEST"
