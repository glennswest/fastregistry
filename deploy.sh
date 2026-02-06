#!/bin/bash
# Deploy FastRegistry to production server
set -e

BINARY="fastregistry-linux"
HOST="fastregistry.gw.lo"
REMOTE_PATH="/usr/local/bin/fastregistry"

echo "=== FastRegistry Deployment ==="

# Build
echo "Building Linux binary..."
GOOS=linux GOARCH=amd64 go build -o "$BINARY" ./cmd/fastregistry

# Get version from binary
VERSION=$(grep -o 'version.*=.*"[0-9.]*"' cmd/fastregistry/main.go | grep -o '[0-9.]*')
echo "Version: $VERSION"

# Stop service
echo "Stopping service on $HOST..."
ssh root@$HOST "systemctl stop fastregistry"

# Upload
echo "Uploading binary..."
scp "$BINARY" root@$HOST:$REMOTE_PATH

# Start service
echo "Starting service..."
ssh root@$HOST "systemctl start fastregistry"

# Verify
echo "Verifying..."
sleep 2
if ssh root@$HOST "systemctl is-active fastregistry" | grep -q "active"; then
    echo "=== Deployment successful ==="
else
    echo "=== WARNING: Service may not be running ==="
    ssh root@$HOST "systemctl status fastregistry"
fi
