#!/usr/bin/env bash

set -euo pipefail

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )/.." >/dev/null 2>&1 && pwd -P )"

# Apply CRDS
kubectl apply -f "$DIR/crds/*.yaml" --server-side

# Create the split load balancer for a single user for testing
kubectl apply -f "$DIR/hack/per-user-service.yaml"

# Create pod that will run binary
kubectl apply -f "$DIR/hack/devbox.yaml"

# Ensure golang, dlv are installed in devbox
kubectl exec devbox-0 -- /bin/bash -c "apt-get update && apt-get install -y golang ca-certificates && go install github.com/go-delve/delve/cmd/dlv@latest"

# Copy binary to devbox-0 pod /app directory
echo "Copying moonlight-proxy to devbox-0..." >&2
kubectl cp moonlight-proxy-linux-amd64 devbox-0:/app/moonlight-proxy

# Kill existing moonlight-proxy process
echo "Killing existing moonlight-proxy process..." >&2
kubectl exec devbox-0 -- pkill dlv || true

# Start moonlight-proxy in background
echo "Starting moonlight-proxy..." >&2
# kubectl exec devbox-0 -- /bin/bash -c "nohup /app/moonlight-proxy > /app/moonlight.log 2>&1 & disown"
# Start running dlv server for moon-light proxy with /app cwd on port 2345 waiting for debugger to attach
kubectl exec devbox-0 -- /bin/bash -c 'cd /app && $(go env GOPATH)/bin/dlv --headless --accept-multiclient --listen=:2345 --log --api-version=2 exec ./moonlight-proxy'

echo "moonlight-proxy started for debugging" >&2
sleep 2
