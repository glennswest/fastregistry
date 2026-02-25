#!/bin/bash
# Build and deploy fastregistry to mkube via registry
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGISTRY="registry.gt.lo:5000"
REPO="fastregistry"

cd "$SCRIPT_DIR"

# Build
"$SCRIPT_DIR/build.sh"

IMAGE_EDGE="$REGISTRY/$REPO:edge"

echo "Pushing to $REGISTRY ..."
podman push --tls-verify=false "$IMAGE_EDGE"

echo ""
echo "=== Deployed ==="
echo "  Image: $IMAGE_EDGE"
echo "  Pod:   fastregistry @ 192.168.10.50:5000"
