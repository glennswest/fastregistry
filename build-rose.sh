#!/bin/bash
set -e

BINARY_NAME="fastregistry"
IMAGE_NAME="fastregistry"
TAR_NAME="${IMAGE_NAME}.tar"

echo "=== Building FastRegistry Image ==="

# Build the Go binary for arm64
echo "Building binary for arm64..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o ${BINARY_NAME} ./cmd/fastregistry

# Build the container image
echo "Building container image..."
podman build --platform linux/arm64 -t ${IMAGE_NAME}:latest -f Dockerfile.rose .

# Save as tarball
echo "Saving image as tarball..."
podman save ${IMAGE_NAME}:latest -o ${TAR_NAME}

# Cleanup binary (it's in the image now)
rm -f ${BINARY_NAME}

echo ""
echo "Build complete: ${TAR_NAME}"
echo "Run ./deploy-rose.sh to deploy to rose1"
