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

# Single multi-stage build (compile + scratch image with CA certs + tzdata).
# The Dockerfile.rose builder stage cross-compiles ARM64 inside the build,
# so no local `go build` step is needed.
podman build --platform linux/arm64 \
  --build-arg VERSION="${FULL_VERSION}" \
  -t "$IMAGE_EDGE" \
  -f Dockerfile.rose .

echo ""
echo "=== Build complete ==="
echo "  $IMAGE_EDGE"
echo "  Version: $FULL_VERSION"
