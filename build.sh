#!/bin/bash
# Build fastregistry: cross-compile locally, then podman build
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGISTRY="registry.gt.lo:5000"
REPO="fastregistry"

cd "$SCRIPT_DIR"

# Get current version and git info
VERSION=$(cat VERSION 2>/dev/null | tr -d '\n' || echo "0.0.0")
GIT_HASH=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
FULL_VERSION="${VERSION}+${GIT_HASH}"
IMAGE_EDGE="$REGISTRY/$REPO:edge"

echo "Building $REPO $FULL_VERSION ..."

# Cross-compile for ARM64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -ldflags="-s -w -X main.version=${FULL_VERSION}" \
  -o fastregistry ./cmd/fastregistry

# Build container image
podman build --platform linux/arm64 -t "$IMAGE_EDGE" -f Dockerfile.rose .

# Clean up binary
rm -f fastregistry

echo ""
echo "=== Build complete ==="
echo "  $IMAGE_EDGE"
echo "  Version: $FULL_VERSION"
