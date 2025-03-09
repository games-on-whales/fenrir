#!/usr/bin/env bash

set -euo pipefail

TARGET="$1"

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )/.." >/dev/null 2>&1 && pwd -P )"

# Apply CRDS
kubectl apply -f "$DIR/crds/*.yaml" --server-side

# Create the split load balancer for a single user for testing
# kubectl apply -f "$DIR/examples/install.yaml" --server-side

# Create pod that will run binary
kubectl apply -f "$DIR/hack/devbox.yaml"

# Ensure golang, dlv are installed in devbox
kubectl exec devbox-0 -- /bin/bash -c "apt-get update && apt-get install -y golang ca-certificates && go install github.com/go-delve/delve/cmd/dlv@latest"

# Copy binary to devbox-0 pod /app directory
echo "Copying $TARGET to devbox-0..." >&2
kubectl cp $TARGET-linux-amd64 devbox-0:/app/$TARGET

# Kill existing $TARGET process
echo "Killing existing $TARGET process..." >&2
kubectl exec devbox-0 -- pkill $TARGET || true

# Start $TARGET in background
echo "Starting $TARGET..." >&2
# kubectl exec devbox-0 -- /bin/bash -c "nohup /app/$TARGET > /app/moonlight.log 2>&1 & disown"
# Start running dlv server for moon-light proxy with /app cwd on port 2345 waiting for debugger to attach
kubectl exec devbox-0 -- /bin/bash -c 'cd /app && $(go env GOPATH)/bin/dlv --headless --accept-multiclient --listen=:2345 --log --api-version=2 exec ./$TARGET'

echo "$TARGET started for debugging" >&2
sleep 2
